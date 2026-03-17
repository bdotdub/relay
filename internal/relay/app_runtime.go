package relay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bdotdub/relay/internal/codex"
	"github.com/bdotdub/relay/internal/logx"
	"github.com/bdotdub/relay/internal/telegram"
)

func (a *relayApp) run(ctx context.Context) error {
	defer a.codex.Close()

	if err := a.loadState(); err != nil {
		return err
	}
	if err := a.telegram.DeleteWebhook(ctx, false); err != nil {
		return err
	}
	if err := a.registerMyCommands(ctx); err != nil {
		logx.Warn("failed to register Telegram slash commands", "error", err)
	}

	var offset *int64
	for {
		updates, err := a.telegram.GetUpdates(ctx, offset, a.cfg.TelegramPollTimeoutSeconds)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		for _, update := range updates {
			nextOffset := update.UpdateID + 1
			offset = &nextOffset
			logx.Debug("telegram update received", "update_id", update.UpdateID)
			if err := a.handleUpdate(ctx, update); err != nil {
				return err
			}
		}
	}
}

func (a *relayApp) registerMyCommands(ctx context.Context) error {
	return a.telegram.SetMyCommands(ctx, []telegram.BotCommand{
		{Command: "help", Description: "show supported commands"},
		{Command: "status", Description: "show current thread and execution status"},
		{Command: "new", Description: "start a new Codex thread"},
		{Command: "reset", Description: "start a new Codex thread"},
		{Command: "verbose", Description: "toggle visible intermediate output"},
		{Command: "yolo", Description: "toggle YOLO execution mode"},
		{Command: "model", Description: "set model override for this chat"},
		{Command: "reload", Description: "reload the running relay process"},
	})
}

func (a *relayApp) handleUpdate(ctx context.Context, update telegram.Update) error {
	if update.Message == nil {
		return nil
	}
	message := update.Message
	logx.Debug("telegram message received",
		"chat_id", message.Chat.ID,
		"message_id", message.MessageID,
		"text", logx.SummarizeText(message.Text),
	)
	if message.Chat.Type != "private" {
		logx.Debug("telegram message ignored", "chat_id", message.Chat.ID, "chat_type", message.Chat.Type, "reason", "chat_type_not_allowed")
		return nil
	}
	if !a.isChatAllowed(message.Chat.ID) {
		logx.Debug("telegram message ignored", "chat_id", message.Chat.ID, "reason", "chat_not_allowed")
		return nil
	}

	// Download image bytes for any attached photo (largest size) and write to a
	// temp file. The bot-token-bearing URL is used internally only; Codex receives
	// only the local file path via the "localImage" input type.
	var imagePaths []string
	if len(message.Photo) > 0 {
		largest := message.Photo[len(message.Photo)-1]
		data, ext, err := a.telegram.DownloadFile(ctx, largest.FileID)
		if err != nil {
			logx.Warn("failed to download telegram photo", "file_id", largest.FileID, "error", err)
			return a.telegram.SendMessage(ctx, message.Chat.ID, "Couldn't download the photo from Telegram. Please try again.")
		}
		tmpFile, err := os.CreateTemp("", "relay-photo-*"+ext)
		if err != nil {
			return fmt.Errorf("create temp file for photo: %w", err)
		}
		tmpPath := tmpFile.Name()
		if _, err := tmpFile.Write(data); err != nil {
			tmpFile.Close()
			if removeErr := os.Remove(tmpPath); removeErr != nil {
				logx.Warn("failed to remove temp photo file", "path", tmpPath, "error", removeErr)
			}
			return fmt.Errorf("write temp photo file: %w", err)
		}
		tmpFile.Close()
		imagePaths = append(imagePaths, tmpPath)
	}

	// Use Caption as the text for photo messages (Telegram stores caption text there).
	text := message.Text
	if text == "" {
		text = message.Caption
	}

	if strings.TrimSpace(text) == "" && len(imagePaths) == 0 {
		return a.telegram.SendMessage(ctx, message.Chat.ID, "Only text and photo messages are supported right now.")
	}

	worker := a.workerForChat(ctx, message.Chat.ID)
	logx.Debug("dispatching message to chat worker", "chat_id", message.Chat.ID, "command", len(text) > 0 && text[0] == '/')
	event := chatEvent{
		messageID:  message.MessageID,
		text:       text,
		imagePaths: imagePaths,
		isCommand:  len(text) > 0 && text[0] == '/',
	}
	if worker.enqueue(event) {
		return nil
	}
	// Queue is full — clean up any temp files we created before dropping the event.
	for _, p := range imagePaths {
		if err := os.Remove(p); err != nil {
			logx.Warn("failed to remove temp photo file", "path", p, "error", err)
		}
	}
	logx.Warn("chat worker queue full; dropping message", "chat_id", message.Chat.ID, "message_id", message.MessageID)
	worker.notifyQueueOverflow()
	return nil
}

func (a *relayApp) workerForChat(ctx context.Context, chatID int64) *chatWorker {
	a.workersMu.Lock()
	defer a.workersMu.Unlock()

	worker := a.workers[chatID]
	if worker != nil {
		return worker
	}

	worker = &chatWorker{
		app:    a,
		chatID: chatID,
		events: make(chan chatEvent, 64),
	}
	a.workers[chatID] = worker
	logx.Info("telegram chat connected", "chat_id", chatID)
	go worker.run(ctx)
	return worker
}

func (w *chatWorker) run(ctx context.Context) {
	var active *activeChatTurn

	for {
		if active == nil {
			select {
			case <-ctx.Done():
				return
			case event := <-w.events:
				nextActive, stop := w.handleEvent(ctx, event, nil)
				if stop {
					return
				}
				active = nextActive
			}
			continue
		}

		select {
		case <-ctx.Done():
			if active != nil && active.stopTyping != nil {
				active.stopTyping()
			}
			return
		case event, ok := <-active.eventCh:
			if !ok {
				active.eventCh = nil
				continue
			}
			w.handleTurnEvent(ctx, active.replyMessageID, event)
		case result, ok := <-active.resultCh:
			if active.stopTyping != nil {
				active.stopTyping()
			}
			if ok {
				w.drainTurnEvents(ctx, active)
				w.finishTurn(ctx, active.replyMessageID, result)
			}
			for _, p := range active.tmpFiles {
				if err := os.Remove(p); err != nil {
					logx.Warn("failed to remove temp photo file", "path", p, "error", err)
				}
			}
			active = nil
		case event := <-w.events:
			nextActive, stop := w.handleEvent(ctx, event, active)
			if stop {
				return
			}
			active = nextActive
		}
	}
}

func (w *chatWorker) enqueue(event chatEvent) bool {
	select {
	case w.events <- event:
		return true
	default:
		return false
	}
}

func (w *chatWorker) notifyQueueOverflow() {
	now := time.Now()

	w.overflowMu.Lock()
	if now.Before(w.nextOverflowNoticeAt) {
		w.overflowMu.Unlock()
		return
	}
	w.nextOverflowNoticeAt = now.Add(5 * time.Second)
	w.overflowMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := w.app.telegram.SendMessage(ctx, w.chatID, "Too many pending messages for this chat. Wait for the current turn to finish, then try again."); err != nil {
			logx.Warn("failed to send queue overflow notice", "chat_id", w.chatID, "error", err)
		}
	}()
}

func (w *chatWorker) handleEvent(ctx context.Context, event chatEvent, active *activeChatTurn) (*activeChatTurn, bool) {
	if event.isCommand {
		logx.Debug("chat worker handling command", "chat_id", w.chatID, "message_id", event.messageID, "command", firstCommandToken(event.text))
		if err := w.handleCommand(ctx, event.messageID, event.text); err != nil {
			w.sendError(ctx, event.messageID, err)
		}
		return active, false
	}

	if active != nil {
		logx.Debug("chat worker steering active turn", "chat_id", w.chatID, "thread_id", active.threadID, "turn_id", active.turnID)
		if err := w.app.codex.SteerTurn(ctx, active.threadID, active.turnID, event.text, event.imagePaths); err != nil {
			w.sendError(ctx, event.messageID, fmt.Errorf("steer codex turn: %w", err))
		} else {
			active.tmpFiles = append(active.tmpFiles, event.imagePaths...)
		}
		return active, false
	}

	nextActive, err := w.startTurn(ctx, event.messageID, event.text, event.imagePaths)
	if err != nil {
		w.sendError(ctx, event.messageID, err)
		return nil, false
	}
	return nextActive, false
}

func (w *chatWorker) startTurn(ctx context.Context, messageID int64, text string, imagePaths []string) (*activeChatTurn, error) {
	options := w.app.threadOptionsForChat(w.chatID)
	threadID, err := w.app.codex.EnsureThread(ctx, w.app.threadIDForChat(w.chatID), options)
	if err != nil {
		return nil, err
	}
	if err := w.app.setThreadIDForChat(w.chatID, threadID); err != nil {
		return nil, err
	}

	handle, err := w.app.codex.StartTurn(ctx, threadID, text, imagePaths)
	if err != nil {
		return nil, err
	}

	logx.Debug("chat worker started turn", "chat_id", w.chatID, "thread_id", handle.ThreadID, "turn_id", handle.TurnID)
	return &activeChatTurn{
		threadID:       handle.ThreadID,
		turnID:         handle.TurnID,
		replyMessageID: messageID,
		eventCh:        handle.EventCh,
		resultCh:       handle.ResultCh,
		stopTyping:     w.startTypingLoop(ctx),
		tmpFiles:       imagePaths,
	}, nil
}

func (w *chatWorker) startTypingLoop(ctx context.Context) func() {
	typingCtx, cancel := context.WithCancel(ctx)
	go func() {
		if err := w.app.telegram.SendChatAction(typingCtx, w.chatID, "typing"); err != nil {
			logx.Debug("telegram typing failed", "chat_id", w.chatID, "error", err)
		}

		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-typingCtx.Done():
				return
			case <-ticker.C:
				if err := w.app.telegram.SendChatAction(typingCtx, w.chatID, "typing"); err != nil {
					logx.Debug("telegram typing failed", "chat_id", w.chatID, "error", err)
				}
			}
		}
	}()
	return cancel
}

func (w *chatWorker) finishTurn(ctx context.Context, replyMessageID int64, result codex.TurnResult) {
	if result.Err != nil {
		w.sendError(ctx, replyMessageID, result.Err)
		return
	}
	w.app.setLastUsageForChat(w.chatID, result.Usage)

	reply := result.Text
	if result.ErrorMessage != "" {
		prefix := "Codex reported an error: " + result.ErrorMessage
		if strings.TrimSpace(reply) == "" {
			reply = prefix
		} else {
			reply = prefix + "\n\n" + reply
		}
	}
	if strings.TrimSpace(reply) == "" {
		reply = "Codex completed the turn without returning assistant text."
	}

	chunks := telegram.ChunkMessage(reply, w.app.cfg.TelegramMessageChunkSize)
	logx.Debug("chat worker replying", "chat_id", w.chatID, "chunks", len(chunks), "text", logx.SummarizeText(reply))
	for _, chunk := range chunks {
		if err := w.app.telegram.SendMessage(ctx, w.chatID, chunk); err != nil {
			return
		}
	}
}

func (w *chatWorker) handleTurnEvent(ctx context.Context, replyMessageID int64, event codex.TurnStreamEvent) {
	if !w.app.verboseForChat(w.chatID) {
		return
	}
	text := strings.TrimSpace(event.Text)
	if text == "" {
		return
	}
	logx.Debug("chat worker intermediate update", "chat_id", w.chatID, "text", logx.SummarizeText(text))
	if err := w.app.telegram.SendMessage(ctx, w.chatID, text); err != nil {
		logx.Debug("chat worker intermediate update failed", "chat_id", w.chatID, "error", err)
	}
}

func (w *chatWorker) drainTurnEvents(ctx context.Context, active *activeChatTurn) {
	for active != nil && active.eventCh != nil {
		select {
		case event, ok := <-active.eventCh:
			if !ok {
				active.eventCh = nil
				return
			}
			w.handleTurnEvent(ctx, active.replyMessageID, event)
		default:
			return
		}
	}
}

func (w *chatWorker) sendError(ctx context.Context, replyMessageID int64, err error) {
	logx.Debug("chat worker error", "chat_id", w.chatID, "error", err)
	_ = w.app.telegram.SendMessage(ctx, w.chatID, fmt.Sprintf("Codex relay error: %v", err))
}

func (a *relayApp) isChatAllowed(chatID int64) bool {
	if len(a.cfg.TelegramAllowedChatIDs) == 0 {
		return false
	}
	_, ok := a.cfg.TelegramAllowedChatIDs[chatID]
	return ok
}
