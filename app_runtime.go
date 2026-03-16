package main

import (
	"context"
	"fmt"
	"time"
)

func (a *relayApp) run(ctx context.Context) error {
	defer a.codex.close()

	if err := a.loadState(); err != nil {
		return err
	}
	if err := a.telegram.deleteWebhook(ctx, false); err != nil {
		return err
	}

	var offset *int64
	for {
		updates, err := a.telegram.getUpdates(ctx, offset, a.cfg.telegramPollTimeoutSeconds)
		if err != nil {
			return err
		}
		for _, update := range updates {
			nextOffset := update.UpdateID + 1
			offset = &nextOffset
			debugf("telegram update received %s", kvSummary("update_id", update.UpdateID))
			if err := a.handleUpdate(ctx, update); err != nil {
				return err
			}
		}
	}
}

func (a *relayApp) handleUpdate(ctx context.Context, update telegramUpdate) error {
	if update.Message == nil {
		return nil
	}
	message := update.Message
	debugf("telegram message received %s", kvSummary(
		"chat_id", message.Chat.ID,
		"message_id", message.MessageID,
		"text", summarizeText(message.Text),
	))
	if !a.isChatAllowed(message.Chat.ID) {
		debugf("telegram message ignored %s", kvSummary("chat_id", message.Chat.ID, "reason", "chat_not_allowed"))
		return nil
	}
	if stringsTrimSpace(message.Text) == "" {
		return a.telegram.sendMessage(ctx, message.Chat.ID, "Only plain text messages are supported right now.")
	}

	worker := a.workerForChat(ctx, message.Chat.ID)
	debugf("dispatching message to chat worker %s", kvSummary("chat_id", message.Chat.ID, "command", len(message.Text) > 0 && message.Text[0] == '/'))
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
	debugf("created chat worker %s", kvSummary("chat_id", chatID))
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
		debugf("chat worker handling command %s", kvSummary("chat_id", w.chatID, "message_id", event.messageID, "command", firstCommandToken(event.text)))
		if err := w.handleCommand(ctx, event.messageID, event.text); err != nil {
			w.sendError(ctx, event.messageID, err)
		}
		return active, false
	}

	if active != nil {
		debugf("chat worker steering active turn %s", kvSummary("chat_id", w.chatID, "thread_id", active.threadID, "turn_id", active.turnID))
		if err := w.app.codex.steerTurn(ctx, active.threadID, active.turnID, event.text); err != nil {
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
	threadID, err := w.app.codex.ensureThread(ctx, w.app.threadIDForChat(w.chatID), options)
	if err != nil {
		return nil, err
	}
	if err := w.app.setThreadIDForChat(w.chatID, threadID); err != nil {
		return nil, err
	}

	handle, err := w.app.codex.startTurn(ctx, threadID, text)
	if err != nil {
		return nil, err
	}

	debugf("chat worker started turn %s", kvSummary("chat_id", w.chatID, "thread_id", handle.ThreadID, "turn_id", handle.TurnID))
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
		if err := w.app.telegram.sendChatAction(typingCtx, w.chatID, "typing"); err != nil {
			debugf("telegram typing failed %s", kvSummary("chat_id", w.chatID, "error", err))
		}

		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-typingCtx.Done():
				return
			case <-ticker.C:
				if err := w.app.telegram.sendChatAction(typingCtx, w.chatID, "typing"); err != nil {
					debugf("telegram typing failed %s", kvSummary("chat_id", w.chatID, "error", err))
				}
			}
		}
	}()
	return cancel
}

func (w *chatWorker) finishTurn(ctx context.Context, replyMessageID int64, result turnResult) {
	if result.err != nil {
		w.sendError(ctx, replyMessageID, result.err)
		return
	}
	w.app.setLastUsageForChat(w.chatID, result.usage)

	reply := result.text
	if result.errorMessage != "" {
		prefix := "Codex reported an error: " + result.errorMessage
		if stringsTrimSpace(reply) == "" {
			reply = prefix
		} else {
			reply = prefix + "\n\n" + reply
		}
	}
	if stringsTrimSpace(reply) == "" {
		reply = "Codex completed the turn without returning assistant text."
	}

	chunks := chunkMessage(reply, w.app.cfg.telegramMessageChunkSize)
	debugf("chat worker replying %s", kvSummary("chat_id", w.chatID, "chunks", len(chunks), "text", summarizeText(reply)))
	for _, chunk := range chunks {
		if err := w.app.telegram.sendMessage(ctx, w.chatID, chunk); err != nil {
			return
		}
	}
}

func (w *chatWorker) handleTurnEvent(ctx context.Context, replyMessageID int64, event turnStreamEvent) {
	if !w.app.verboseForChat(w.chatID) {
		return
	}
	text := stringsTrimSpace(event.text)
	if text == "" {
		return
	}
	debugf("chat worker intermediate update %s", kvSummary("chat_id", w.chatID, "text", summarizeText(text)))
	if err := w.app.telegram.sendMessage(ctx, w.chatID, text); err != nil {
		debugf("chat worker intermediate update failed %s", kvSummary("chat_id", w.chatID, "error", err))
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
	debugf("chat worker error %s", kvSummary("chat_id", w.chatID, "error", err))
	_ = w.app.telegram.sendMessage(ctx, w.chatID, fmt.Sprintf("Codex relay error: %v", err))
}

func (a *relayApp) isChatAllowed(chatID int64) bool {
	if a.cfg.telegramAllowedChatIDs == nil {
		return true
	}
	_, ok := a.cfg.telegramAllowedChatIDs[chatID]
	return ok
}
