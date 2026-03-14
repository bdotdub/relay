package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type relayApp struct {
	cfg             config
	telegram        *telegramClient
	codex           *codexClient
	threadIDsByChat map[string]string
}

func newRelayApp(cfg config) (*relayApp, error) {
	telegram := newTelegramClient(cfg.telegramBotToken)
	codex, err := newCodexClient(cfg)
	if err != nil {
		return nil, err
	}
	return &relayApp{
		cfg:             cfg,
		telegram:        telegram,
		codex:           codex,
		threadIDsByChat: map[string]string{},
	}, nil
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
	if !a.isChatAllowed(message.Chat.ID) {
		return nil
	}
	if stringsTrimSpace(message.Text) == "" {
		replyTo := message.MessageID
		return a.telegram.sendMessage(ctx, message.Chat.ID, "Only plain text messages are supported right now.", &replyTo)
	}
	if len(message.Text) > 0 && message.Text[0] == '/' {
		return a.handleCommand(ctx, message.Chat.ID, message.MessageID, message.Text)
	}
	return a.relayMessage(ctx, message.Chat.ID, message.MessageID, message.Text)
}

func (a *relayApp) handleCommand(ctx context.Context, chatID int64, messageID int64, text string) error {
	command := firstCommandToken(text)
	replyTo := messageID

	switch command {
	case "/new", "/reset":
		threadID, err := a.codex.newThread(ctx)
		if err != nil {
			return err
		}
		a.threadIDsByChat[fmt.Sprintf("%d", chatID)] = threadID
		if err := a.saveState(); err != nil {
			return err
		}
		return a.telegram.sendMessage(ctx, chatID, fmt.Sprintf("Started a new Codex thread.\nthread_id=%s", threadID), &replyTo)
	case "/status":
		threadID := a.threadIDsByChat[fmt.Sprintf("%d", chatID)]
		if threadID == "" {
			threadID = "(not started yet)"
		}
		mode := "stdio subprocess"
		if !a.cfg.codexStartAppServer {
			mode = "websocket"
		}
		return a.telegram.sendMessage(ctx, chatID, fmt.Sprintf("Transport: %s\nThread: %s\nCWD: %s", mode, threadID, a.cfg.codexCWD), &replyTo)
	case "/help":
		return a.telegram.sendMessage(ctx, chatID, "Send any text message to relay it to Codex.\n/new or /reset starts a fresh Codex thread.\n/status shows the current thread mapping.", &replyTo)
	default:
		return a.telegram.sendMessage(ctx, chatID, "Unknown command. Use /help for the supported commands.", &replyTo)
	}
}

func (a *relayApp) relayMessage(ctx context.Context, chatID int64, messageID int64, text string) error {
	chatKey := fmt.Sprintf("%d", chatID)
	threadID, err := a.codex.ensureThread(ctx, a.threadIDsByChat[chatKey])
	if err != nil {
		return err
	}
	if a.threadIDsByChat[chatKey] != threadID {
		a.threadIDsByChat[chatKey] = threadID
		if err := a.saveState(); err != nil {
			return err
		}
	}

	result, err := a.codex.runTurn(ctx, threadID, text)
	if err != nil {
		replyTo := messageID
		return a.telegram.sendMessage(ctx, chatID, fmt.Sprintf("Codex relay error: %v", err), &replyTo)
	}

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

	chunks := chunkMessage(reply, a.cfg.telegramMessageChunkSize)
	for index, chunk := range chunks {
		var replyTo *int64
		if index == 0 {
			replyTo = &messageID
		}
		if err := a.telegram.sendMessage(ctx, chatID, chunk, replyTo); err != nil {
			return err
		}
	}
	return nil
}

func (a *relayApp) isChatAllowed(chatID int64) bool {
	if a.cfg.telegramAllowedChatIDs == nil {
		return true
	}
	_, ok := a.cfg.telegramAllowedChatIDs[chatID]
	return ok
}

func (a *relayApp) loadState() error {
	data, err := os.ReadFile(a.cfg.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			a.threadIDsByChat = map[string]string{}
			return nil
		}
		return fmt.Errorf("read relay state: %w", err)
	}
	var mapping map[string]string
	if err := json.Unmarshal(data, &mapping); err != nil {
		a.threadIDsByChat = map[string]string{}
		return nil
	}
	a.threadIDsByChat = mapping
	return nil
}

func (a *relayApp) saveState() error {
	if err := os.MkdirAll(filepath.Dir(a.cfg.statePath), 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	data, err := json.MarshalIndent(a.threadIDsByChat, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal relay state: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(a.cfg.statePath, data, 0o644); err != nil {
		return fmt.Errorf("write relay state: %w", err)
	}
	return nil
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
