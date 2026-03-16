package relay

import (
	"context"
	"fmt"
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

	var offset *int64
	for {
		updates, err := a.telegram.GetUpdates(ctx, offset, a.cfg.TelegramPollTimeoutSeconds)
		if err != nil {
			return err
		}
		for _, update := range updates {
			nextOffset := update.UpdateID + 1
			offset = &nextOffset
			logx.Debugf("telegram update received %s", logx.KVSummary("update_id", update.UpdateID))
			if err := a.handleUpdate(ctx, update); err != nil {
				return err
			}
		}
	}
}

func (a *relayApp) handleUpdate(ctx context.Context, update telegram.Update) error {
	if update.Message == nil {
		return nil
	}
	message := update.Message
	logx.Debugf("telegram message received %s", logx.KVSummary(
		"chat_id", message.Chat.ID,
		"message_id", message.MessageID,
		"text", logx.SummarizeText(message.Text),
	))
	if !a.isChatAllowed(message.Chat.ID) {
		logx.Debugf("telegram message ignored %s", logx.KVSummary("chat_id", message.Chat.ID, "reason", "chat_not_allowed"))
		return nil
	}
	if strings.TrimSpace(message.Text) == "" {
		return a.telegram.SendMessage(ctx, message.Chat.ID, "Only plain text messages are supported right now.")
	}

	worker := a.workerForChat(ctx, message.Chat.ID)
	logx.Debugf("dispatching message to chat worker %s", logx.KVSummary("chat_id", message.Chat.ID, "command", len(message.Text) > 0 && message.Text[0] == '/'))
	worker.events <- chatEvent{
		messageID: message.MessageID,
		text:      message.Text,
		isCommand: len(message.Text) > 0 && message.Text[0] == '/',
	}
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
	logx.Infof("telegram chat connected %s", logx.KVSummary("chat_id", chatID))
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

func (w *chatWorker) handleEvent(ctx context.Context, event chatEvent, active *activeChatTurn) (*activeChatTurn, bool) {
	if event.isCommand {
		logx.Debugf("chat worker handling command %s", logx.KVSummary("chat_id", w.chatID, "message_id", event.messageID, "command", firstCommandToken(event.text)))
		if err := w.handleCommand(ctx, event.messageID, event.text); err != nil {
			w.sendError(ctx, event.messageID, err)
		}
		return active, false
	}

	if active != nil {
		logx.Debugf("chat worker steering active turn %s", logx.KVSummary("chat_id", w.chatID, "thread_id", active.threadID, "turn_id", active.turnID))
		if err := w.app.codex.SteerTurn(ctx, active.threadID, active.turnID, event.text); err != nil {
			w.sendError(ctx, event.messageID, fmt.Errorf("steer codex turn: %w", err))
		}
		return active, false
	}

	nextActive, err := w.startTurn(ctx, event.messageID, event.text)
	if err != nil {
		w.sendError(ctx, event.messageID, err)
		return nil, false
	}
	return nextActive, false
}

func (w *chatWorker) startTurn(ctx context.Context, messageID int64, text string) (*activeChatTurn, error) {
	options := w.app.threadOptionsForChat(w.chatID)
	threadID, err := w.app.codex.EnsureThread(ctx, w.app.threadIDForChat(w.chatID), options)
	if err != nil {
		return nil, err
	}
	if err := w.app.setThreadIDForChat(w.chatID, threadID); err != nil {
		return nil, err
	}

	handle, err := w.app.codex.StartTurn(ctx, threadID, text)
	if err != nil {
		return nil, err
	}

	logx.Debugf("chat worker started turn %s", logx.KVSummary("chat_id", w.chatID, "thread_id", handle.ThreadID, "turn_id", handle.TurnID))
	return &activeChatTurn{
		threadID:       handle.ThreadID,
		turnID:         handle.TurnID,
		replyMessageID: messageID,
		eventCh:        handle.EventCh,
		resultCh:       handle.ResultCh,
		stopTyping:     w.startTypingLoop(ctx),
	}, nil
}

func (w *chatWorker) startTypingLoop(ctx context.Context) func() {
	typingCtx, cancel := context.WithCancel(ctx)
	go func() {
		if err := w.app.telegram.SendChatAction(typingCtx, w.chatID, "typing"); err != nil {
			logx.Debugf("telegram typing failed %s", logx.KVSummary("chat_id", w.chatID, "error", err))
		}

		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-typingCtx.Done():
				return
			case <-ticker.C:
				if err := w.app.telegram.SendChatAction(typingCtx, w.chatID, "typing"); err != nil {
					logx.Debugf("telegram typing failed %s", logx.KVSummary("chat_id", w.chatID, "error", err))
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
	logx.Debugf("chat worker replying %s", logx.KVSummary("chat_id", w.chatID, "chunks", len(chunks), "text", logx.SummarizeText(reply)))
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
	logx.Debugf("chat worker intermediate update %s", logx.KVSummary("chat_id", w.chatID, "text", logx.SummarizeText(text)))
	if err := w.app.telegram.SendMessage(ctx, w.chatID, text); err != nil {
		logx.Debugf("chat worker intermediate update failed %s", logx.KVSummary("chat_id", w.chatID, "error", err))
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
	logx.Debugf("chat worker error %s", logx.KVSummary("chat_id", w.chatID, "error", err))
	_ = w.app.telegram.SendMessage(ctx, w.chatID, fmt.Sprintf("Codex relay error: %v", err))
}

func (a *relayApp) isChatAllowed(chatID int64) bool {
	if a.cfg.TelegramAllowedChatIDs == nil {
		return true
	}
	_, ok := a.cfg.TelegramAllowedChatIDs[chatID]
	return ok
}
