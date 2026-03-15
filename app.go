package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type telegramService interface {
	deleteWebhook(ctx context.Context, dropPending bool) error
	getUpdates(ctx context.Context, offset *int64, timeoutSeconds int) ([]telegramUpdate, error)
	sendMessage(ctx context.Context, chatID int64, text string) error
	sendChatAction(ctx context.Context, chatID int64, action string) error
}

type relayApp struct {
	cfg      config
	telegram telegramService
	codex    codexService

	stateMu         sync.RWMutex
	threadIDsByChat map[string]string
	verboseByChat   map[int64]bool
	yoloByChat      map[int64]bool
	modelByChat     map[int64]string
	lastUsageByChat map[int64]tokenUsage

	workersMu sync.Mutex
	workers   map[int64]*chatWorker
}

type chatWorker struct {
	app    *relayApp
	chatID int64
	events chan chatEvent
}

type chatEvent struct {
	messageID int64
	text      string
	isCommand bool
}

type activeChatTurn struct {
	threadID       string
	turnID         string
	replyMessageID int64
	eventCh        <-chan turnStreamEvent
	resultCh       <-chan turnResult
	stopTyping     func()
}

type relayState struct {
	Threads     map[string]string `json:"threads,omitempty"`
	YoloByChat  map[string]bool   `json:"yolo_by_chat,omitempty"`
	ModelByChat map[string]string `json:"model_by_chat,omitempty"`
}

func newRelayApp(cfg config) (*relayApp, error) {
	telegram := newTelegramClient(cfg.telegramBotToken)
	codex, err := newCodexClient(cfg)
	if err != nil {
		return nil, err
	}
	return newRelayAppWithServices(cfg, telegram, codex), nil
}

func newRelayAppWithServices(cfg config, telegram telegramService, codex codexService) *relayApp {
	return &relayApp{
		cfg:             cfg,
		telegram:        telegram,
		codex:           codex,
		threadIDsByChat: map[string]string{},
		verboseByChat:   map[int64]bool{},
		yoloByChat:      map[int64]bool{},
		modelByChat:     map[int64]string{},
		lastUsageByChat: map[int64]tokenUsage{},
		workers:         map[int64]*chatWorker{},
	}
}

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
			verbosef("telegram update received %s", kvSummary("update_id", update.UpdateID))
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
	verbosef("telegram message received %s", kvSummary(
		"chat_id", message.Chat.ID,
		"message_id", message.MessageID,
		"text", summarizeText(message.Text),
	))
	if !a.isChatAllowed(message.Chat.ID) {
		verbosef("telegram message ignored %s", kvSummary("chat_id", message.Chat.ID, "reason", "chat_not_allowed"))
		return nil
	}
	if stringsTrimSpace(message.Text) == "" {
		return a.telegram.sendMessage(ctx, message.Chat.ID, "Only plain text messages are supported right now.")
	}

	worker := a.workerForChat(ctx, message.Chat.ID)
	verbosef("dispatching message to chat worker %s", kvSummary("chat_id", message.Chat.ID, "command", len(message.Text) > 0 && message.Text[0] == '/'))
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
	verbosef("created chat worker %s", kvSummary("chat_id", chatID))
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
		verbosef("chat worker handling command %s", kvSummary("chat_id", w.chatID, "message_id", event.messageID, "command", firstCommandToken(event.text)))
		if err := w.handleCommand(ctx, event.messageID, event.text); err != nil {
			w.sendError(ctx, event.messageID, err)
		}
		return active, false
	}

	if active != nil {
		verbosef("chat worker steering active turn %s", kvSummary("chat_id", w.chatID, "thread_id", active.threadID, "turn_id", active.turnID))
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

	verbosef("chat worker started turn %s", kvSummary("chat_id", w.chatID, "thread_id", handle.ThreadID, "turn_id", handle.TurnID))
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
			verbosef("telegram typing failed %s", kvSummary("chat_id", w.chatID, "error", err))
		}

		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-typingCtx.Done():
				return
			case <-ticker.C:
				if err := w.app.telegram.sendChatAction(typingCtx, w.chatID, "typing"); err != nil {
					verbosef("telegram typing failed %s", kvSummary("chat_id", w.chatID, "error", err))
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
	verbosef("chat worker replying %s", kvSummary("chat_id", w.chatID, "chunks", len(chunks), "text", summarizeText(reply)))
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
	verbosef("chat worker intermediate update %s", kvSummary("chat_id", w.chatID, "text", summarizeText(text)))
	if err := w.app.telegram.sendMessage(ctx, w.chatID, text); err != nil {
		verbosef("chat worker intermediate update failed %s", kvSummary("chat_id", w.chatID, "error", err))
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

func (w *chatWorker) handleCommand(ctx context.Context, messageID int64, text string) error {
	command := firstCommandToken(text)

	switch command {
	case "/new", "/reset":
		options := w.app.threadOptionsForChat(w.chatID)
		threadID, err := w.app.codex.newThread(ctx, options)
		if err != nil {
			return err
		}
		if err := w.app.setThreadIDForChat(w.chatID, threadID); err != nil {
			return err
		}
		return w.app.telegram.sendMessage(ctx, w.chatID, fmt.Sprintf("Started a new Codex thread in %s mode with model %s.\nthread_id=%s", w.app.executionModeName(w.chatID), w.app.modelForChat(w.chatID), threadID))
	case "/status":
		threadID := w.app.threadIDForChat(w.chatID)
		if threadID == "" {
			threadID = "(not started yet)"
		}
		mode := "stdio subprocess"
		if !w.app.cfg.codexStartAppServer {
			mode = "websocket"
		}
		return w.app.telegram.sendMessage(ctx, w.chatID, fmt.Sprintf("Transport: %s\nExecution: %s\nModel: %s\nThread: %s\nCWD: %s\nTokens: %s", mode, w.app.executionProfileSummary(w.chatID), w.app.modelSummaryForChat(w.chatID), threadID, w.app.cfg.codexCWD, formatTokenUsage(w.app.lastUsageForChat(w.chatID))))
	case "/help":
		return w.app.telegram.sendMessage(ctx, w.chatID, "Send any text message to relay it to Codex.\n/new or /reset starts a fresh Codex thread.\n/status shows the current thread mapping, execution mode, model, and last token usage.\n/verbose toggles intermediate visible output for this chat.\n/yolo toggles YOLO execution mode for this chat and starts a fresh thread.\n/model sets a per-chat model override and starts a fresh thread.")
	case "/verbose":
		enabled, message := w.app.toggleVerboseForChat(w.chatID, text)
		if message == "" {
			if enabled {
				message = "Verbose mode enabled for this chat."
			} else {
				message = "Verbose mode disabled for this chat."
			}
		}
		return w.app.telegram.sendMessage(ctx, w.chatID, message)
	case "/yolo":
		enabled, changed, message := w.app.toggleYoloForChat(w.chatID, text)
		if message != "" {
			return w.app.telegram.sendMessage(ctx, w.chatID, message)
		}
		if !changed {
			return w.app.telegram.sendMessage(ctx, w.chatID, fmt.Sprintf("YOLO mode is already %s for this chat.", enabledDisabled(enabled)))
		}
		threadID, err := w.app.codex.newThread(ctx, codexThreadOptions{yolo: enabled})
		if err != nil {
			return err
		}
		if err := w.app.setThreadIDForChat(w.chatID, threadID); err != nil {
			return err
		}
		return w.app.telegram.sendMessage(ctx, w.chatID, fmt.Sprintf("YOLO mode %s for this chat. Started a fresh Codex thread with model %s.\nthread_id=%s", enabledDisabled(enabled), w.app.modelForChat(w.chatID), threadID))
	case "/model":
		model, changed, message := w.app.setModelForChat(w.chatID, text)
		if message != "" {
			return w.app.telegram.sendMessage(ctx, w.chatID, message)
		}
		if !changed {
			return w.app.telegram.sendMessage(ctx, w.chatID, fmt.Sprintf("Model is already %s for this chat.", w.app.modelSummaryForChat(w.chatID)))
		}
		threadID, err := w.app.codex.newThread(ctx, w.app.threadOptionsForChat(w.chatID))
		if err != nil {
			return err
		}
		if err := w.app.setThreadIDForChat(w.chatID, threadID); err != nil {
			return err
		}
		return w.app.telegram.sendMessage(ctx, w.chatID, fmt.Sprintf("Model set to %s for this chat. Started a fresh Codex thread.\nthread_id=%s", model, threadID))
	default:
		return w.app.telegram.sendMessage(ctx, w.chatID, "Unknown command. Use /help for the supported commands.")
	}
}

func (w *chatWorker) sendError(ctx context.Context, replyMessageID int64, err error) {
	verbosef("chat worker error %s", kvSummary("chat_id", w.chatID, "error", err))
	_ = w.app.telegram.sendMessage(ctx, w.chatID, fmt.Sprintf("Codex relay error: %v", err))
}

func (a *relayApp) isChatAllowed(chatID int64) bool {
	if a.cfg.telegramAllowedChatIDs == nil {
		return true
	}
	_, ok := a.cfg.telegramAllowedChatIDs[chatID]
	return ok
}

func (a *relayApp) threadIDForChat(chatID int64) string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.threadIDsByChat[fmt.Sprintf("%d", chatID)]
}

func (a *relayApp) verboseForChat(chatID int64) bool {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.verboseByChat[chatID]
}

func (a *relayApp) yoloForChat(chatID int64) bool {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.yoloByChat[chatID]
}

func (a *relayApp) modelOverrideForChat(chatID int64) string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.modelByChat[chatID]
}

func (a *relayApp) lastUsageForChat(chatID int64) *tokenUsage {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	usage, ok := a.lastUsageByChat[chatID]
	if !ok {
		return nil
	}
	copy := usage
	return &copy
}

func (a *relayApp) threadOptionsForChat(chatID int64) codexThreadOptions {
	return codexThreadOptions{
		yolo:  a.yoloForChat(chatID),
		model: a.modelForChat(chatID),
	}
}

func (a *relayApp) executionModeName(chatID int64) string {
	if a.yoloForChat(chatID) {
		return "YOLO"
	}
	return "default"
}

func (a *relayApp) executionProfileSummary(chatID int64) string {
	if a.yoloForChat(chatID) {
		return "YOLO (approval=never, sandbox=danger-full-access)"
	}
	return fmt.Sprintf("default (approval=%s, sandbox=%s)", defaultString(a.cfg.codexApprovalPolicy, "(unset)"), defaultString(a.cfg.codexSandbox, "(unset)"))
}

func (a *relayApp) modelForChat(chatID int64) string {
	model := a.modelOverrideForChat(chatID)
	if stringsTrimSpace(model) != "" {
		return model
	}
	return defaultString(a.cfg.codexModel, "spark")
}

func (a *relayApp) modelSummaryForChat(chatID int64) string {
	model := a.modelForChat(chatID)
	if stringsTrimSpace(a.modelOverrideForChat(chatID)) == "" {
		return fmt.Sprintf("%s (default)", model)
	}
	return fmt.Sprintf("%s (override)", model)
}

func (a *relayApp) toggleVerboseForChat(chatID int64, text string) (bool, string) {
	command := stringsTrimSpace(text)
	arg := ""
	if index := stringsIndexAny(command, " \t\r\n"); index >= 0 {
		arg = stringsTrimSpace(command[index+1:])
	}

	a.stateMu.Lock()
	defer a.stateMu.Unlock()

	switch arg {
	case "", "toggle":
		a.verboseByChat[chatID] = !a.verboseByChat[chatID]
	case "on":
		a.verboseByChat[chatID] = true
	case "off":
		a.verboseByChat[chatID] = false
	case "status":
		if a.verboseByChat[chatID] {
			return true, "Verbose mode is enabled for this chat."
		}
		return false, "Verbose mode is disabled for this chat."
	default:
		return a.verboseByChat[chatID], "Usage: /verbose, /verbose on, /verbose off, or /verbose status"
	}
	verbosef("chat verbose mode changed %s", kvSummary("chat_id", chatID, "enabled", a.verboseByChat[chatID]))

	if a.verboseByChat[chatID] {
		return true, ""
	}
	return false, ""
}

func (a *relayApp) toggleYoloForChat(chatID int64, text string) (bool, bool, string) {
	command := stringsTrimSpace(text)
	arg := ""
	if index := stringsIndexAny(command, " \t\r\n"); index >= 0 {
		arg = stringsTrimSpace(command[index+1:])
	}

	a.stateMu.Lock()
	current := a.yoloByChat[chatID]
	next := current
	switch arg {
	case "", "toggle":
		next = !current
	case "on":
		next = true
	case "off":
		next = false
	case "status":
		a.stateMu.Unlock()
		return current, false, fmt.Sprintf("YOLO mode is %s for this chat.", enabledDisabled(current))
	default:
		a.stateMu.Unlock()
		return current, false, "Usage: /yolo, /yolo on, /yolo off, or /yolo status"
	}

	if next == current {
		a.stateMu.Unlock()
		return current, false, ""
	}
	a.yoloByChat[chatID] = next
	a.stateMu.Unlock()

	verbosef("chat yolo mode changed %s", kvSummary("chat_id", chatID, "enabled", next))
	if err := a.saveState(); err != nil {
		a.stateMu.Lock()
		if current {
			a.yoloByChat[chatID] = true
		} else {
			delete(a.yoloByChat, chatID)
		}
		a.stateMu.Unlock()
		return current, false, fmt.Sprintf("Failed to update YOLO mode: %v", err)
	}
	return next, true, ""
}

func (a *relayApp) setModelForChat(chatID int64, text string) (string, bool, string) {
	command := stringsTrimSpace(text)
	arg := ""
	if index := stringsIndexAny(command, " \t\r\n"); index >= 0 {
		arg = stringsTrimSpace(command[index+1:])
	}
	if arg == "" || arg == "status" {
		return a.modelSummaryForChat(chatID), false, fmt.Sprintf("Model is %s for this chat.", a.modelSummaryForChat(chatID))
	}

	nextOverride := arg
	switch strings.ToLower(arg) {
	case "default", "reset", "clear":
		nextOverride = ""
	}

	a.stateMu.Lock()
	currentOverride := a.modelByChat[chatID]
	if currentOverride == nextOverride {
		a.stateMu.Unlock()
		return a.modelSummaryForChat(chatID), false, ""
	}
	if nextOverride == "" {
		delete(a.modelByChat, chatID)
	} else {
		a.modelByChat[chatID] = nextOverride
	}
	a.stateMu.Unlock()

	verbosef("chat model changed %s", kvSummary("chat_id", chatID, "model", defaultString(nextOverride, a.cfg.codexModel)))
	if err := a.saveState(); err != nil {
		a.stateMu.Lock()
		if currentOverride == "" {
			delete(a.modelByChat, chatID)
		} else {
			a.modelByChat[chatID] = currentOverride
		}
		a.stateMu.Unlock()
		return a.modelSummaryForChat(chatID), false, fmt.Sprintf("Failed to update model: %v", err)
	}
	return a.modelForChat(chatID), true, ""
}

func (a *relayApp) setThreadIDForChat(chatID int64, threadID string) error {
	chatKey := fmt.Sprintf("%d", chatID)
	a.stateMu.Lock()
	changed := a.threadIDsByChat[chatKey] != threadID
	if changed {
		a.threadIDsByChat[chatKey] = threadID
		delete(a.lastUsageByChat, chatID)
	}
	a.stateMu.Unlock()
	if changed {
		return a.saveState()
	}
	return nil
}

func (a *relayApp) setLastUsageForChat(chatID int64, usage *tokenUsage) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if usage == nil {
		delete(a.lastUsageByChat, chatID)
		return
	}
	a.lastUsageByChat[chatID] = *usage
}

func (a *relayApp) loadState() error {
	data, err := os.ReadFile(a.cfg.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			a.stateMu.Lock()
			a.threadIDsByChat = map[string]string{}
			a.yoloByChat = map[int64]bool{}
			a.modelByChat = map[int64]string{}
			a.stateMu.Unlock()
			return nil
		}
		return fmt.Errorf("read relay state: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err == nil {
		if _, hasThreads := raw["threads"]; hasThreads || raw["yolo_by_chat"] != nil || raw["model_by_chat"] != nil {
			var state relayState
			if err := json.Unmarshal(data, &state); err != nil {
				a.stateMu.Lock()
				a.threadIDsByChat = map[string]string{}
				a.yoloByChat = map[int64]bool{}
				a.modelByChat = map[int64]string{}
				a.stateMu.Unlock()
				return nil
			}
			a.stateMu.Lock()
			a.threadIDsByChat = state.Threads
			if a.threadIDsByChat == nil {
				a.threadIDsByChat = map[string]string{}
			}
			a.yoloByChat = decodeBoolMap(state.YoloByChat)
			a.modelByChat = decodeStringMap(state.ModelByChat)
			a.stateMu.Unlock()
			return nil
		}
	}

	var mapping map[string]string
	if err := json.Unmarshal(data, &mapping); err != nil {
		a.stateMu.Lock()
		a.threadIDsByChat = map[string]string{}
		a.yoloByChat = map[int64]bool{}
		a.modelByChat = map[int64]string{}
		a.stateMu.Unlock()
		return nil
	}
	a.stateMu.Lock()
	a.threadIDsByChat = mapping
	a.yoloByChat = map[int64]bool{}
	a.modelByChat = map[int64]string{}
	a.stateMu.Unlock()
	return nil
}

func (a *relayApp) saveState() error {
	if err := os.MkdirAll(filepath.Dir(a.cfg.statePath), 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	a.stateMu.RLock()
	state := relayState{
		Threads:     a.threadIDsByChat,
		YoloByChat:  encodeBoolMap(a.yoloByChat),
		ModelByChat: encodeStringMap(a.modelByChat),
	}
	data, err := json.MarshalIndent(state, "", "  ")
	a.stateMu.RUnlock()
	if err != nil {
		return fmt.Errorf("marshal relay state: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(a.cfg.statePath, data, 0o644); err != nil {
		return fmt.Errorf("write relay state: %w", err)
	}
	return nil
}

func encodeBoolMap(values map[int64]bool) map[string]bool {
	if len(values) == 0 {
		return nil
	}
	encoded := make(map[string]bool, len(values))
	for chatID, enabled := range values {
		if !enabled {
			continue
		}
		encoded[fmt.Sprintf("%d", chatID)] = true
	}
	if len(encoded) == 0 {
		return nil
	}
	return encoded
}

func decodeBoolMap(values map[string]bool) map[int64]bool {
	if len(values) == 0 {
		return map[int64]bool{}
	}
	decoded := make(map[int64]bool, len(values))
	for rawChatID, enabled := range values {
		if !enabled {
			continue
		}
		var chatID int64
		if _, err := fmt.Sscanf(rawChatID, "%d", &chatID); err != nil {
			continue
		}
		decoded[chatID] = true
	}
	return decoded
}

func encodeStringMap(values map[int64]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	encoded := make(map[string]string, len(values))
	for chatID, value := range values {
		value = stringsTrimSpace(value)
		if value == "" {
			continue
		}
		encoded[fmt.Sprintf("%d", chatID)] = value
	}
	if len(encoded) == 0 {
		return nil
	}
	return encoded
}

func decodeStringMap(values map[string]string) map[int64]string {
	if len(values) == 0 {
		return map[int64]string{}
	}
	decoded := make(map[int64]string, len(values))
	for rawChatID, value := range values {
		value = stringsTrimSpace(value)
		if value == "" {
			continue
		}
		var chatID int64
		if _, err := fmt.Sscanf(rawChatID, "%d", &chatID); err != nil {
			continue
		}
		decoded[chatID] = value
	}
	return decoded
}

func enabledDisabled(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func defaultString(value string, fallback string) string {
	if stringsTrimSpace(value) == "" {
		return fallback
	}
	return value
}

func firstCommandToken(text string) string {
	text = stringsTrimSpace(text)
	if text == "" {
		return ""
	}
	token := text
	if index := stringsIndexAny(token, " \t\r\n"); index >= 0 {
		token = token[:index]
	}
	if index := stringsIndex(token, "@"); index >= 0 {
		token = token[:index]
	}
	return token
}

func formatTokenUsage(usage *tokenUsage) string {
	if usage == nil {
		return "(not available yet)"
	}
	return summarizeTokenUsage(usage)
}
