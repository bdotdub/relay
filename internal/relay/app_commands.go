package relay

import (
	"context"
	"fmt"
	"strings"

	"github.com/bdotdub/relay/internal/codex"
)

func (w *chatWorker) handleCommand(ctx context.Context, messageID int64, text string) error {
	command := firstCommandToken(text)

	switch command {
	case "/new", "/reset":
		options := w.app.threadOptionsForChat(w.chatID)
		threadID, err := w.app.codex.NewThread(ctx, options)
		if err != nil {
			return err
		}
		if err := w.app.setThreadIDForChat(w.chatID, threadID); err != nil {
			return err
		}
		return w.app.telegram.SendMessage(ctx, w.chatID, fmt.Sprintf("Started a new Codex thread in %s mode with model %s.\nthread_id=%s", w.app.executionModeName(w.chatID), w.app.modelForChat(w.chatID), threadID))
	case "/status":
		threadID := w.app.threadIDForChat(w.chatID)
		if threadID == "" {
			threadID = "(not started yet)"
		}
		mode := "stdio subprocess"
		if !w.app.cfg.CodexStartAppServer {
			mode = "websocket"
		}
		return w.app.telegram.SendMessage(ctx, w.chatID, fmt.Sprintf("Transport: %s\nExecution: %s\nModel: %s\nThread: %s\nCWD: %s\nTokens: %s", mode, w.app.executionProfileSummary(w.chatID), w.app.modelSummaryForChat(w.chatID), threadID, w.app.cfg.CodexCWD, formatTokenUsage(w.app.lastUsageForChat(w.chatID))))
	case "/help":
		return w.app.telegram.SendMessage(ctx, w.chatID, "Send any text message to relay it to Codex.\n/new or /reset starts a fresh Codex thread.\n/status shows the current thread mapping, execution mode, model, and last token usage.\n/verbose toggles intermediate visible output for this chat.\n/yolo toggles YOLO execution mode for this chat and starts a fresh thread.\n/model sets a per-chat model override and starts a fresh thread.")
	case "/verbose":
		enabled, message := w.app.toggleVerboseForChat(w.chatID, text)
		if message == "" {
			if enabled {
				message = "Verbose mode enabled for this chat."
			} else {
				message = "Verbose mode disabled for this chat."
			}
		}
		return w.app.telegram.SendMessage(ctx, w.chatID, message)
	case "/yolo":
		enabled, changed, message := w.app.toggleYoloForChat(w.chatID, text)
		if message != "" {
			return w.app.telegram.SendMessage(ctx, w.chatID, message)
		}
		if !changed {
			return w.app.telegram.SendMessage(ctx, w.chatID, fmt.Sprintf("YOLO mode is already %s for this chat.", enabledDisabled(enabled)))
		}
		threadID, err := w.app.codex.NewThread(ctx, codex.ThreadOptions{Yolo: enabled})
		if err != nil {
			return err
		}
		if err := w.app.setThreadIDForChat(w.chatID, threadID); err != nil {
			return err
		}
		return w.app.telegram.SendMessage(ctx, w.chatID, fmt.Sprintf("YOLO mode %s for this chat. Started a fresh Codex thread with model %s.\nthread_id=%s", enabledDisabled(enabled), w.app.modelForChat(w.chatID), threadID))
	case "/model":
		model, changed, message := w.app.setModelForChat(w.chatID, text)
		if message != "" {
			return w.app.telegram.SendMessage(ctx, w.chatID, message)
		}
		if !changed {
			return w.app.telegram.SendMessage(ctx, w.chatID, fmt.Sprintf("Model is already %s for this chat.", w.app.modelSummaryForChat(w.chatID)))
		}
		threadID, err := w.app.codex.NewThread(ctx, w.app.threadOptionsForChat(w.chatID))
		if err != nil {
			return err
		}
		if err := w.app.setThreadIDForChat(w.chatID, threadID); err != nil {
			return err
		}
		return w.app.telegram.SendMessage(ctx, w.chatID, fmt.Sprintf("Model set to %s for this chat. Started a fresh Codex thread.\nthread_id=%s", model, threadID))
	default:
		return w.app.telegram.SendMessage(ctx, w.chatID, "Unknown command. Use /help for the supported commands.")
	}
}

func (a *relayApp) threadOptionsForChat(chatID int64) codex.ThreadOptions {
	return codex.ThreadOptions{
		Yolo:  a.yoloForChat(chatID),
		Model: a.modelForChat(chatID),
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
	return fmt.Sprintf("default (approval=%s, sandbox=%s)", defaultString(a.cfg.CodexApprovalPolicy, "(unset)"), defaultString(a.cfg.CodexSandbox, "(unset)"))
}

func (a *relayApp) modelForChat(chatID int64) string {
	model := a.modelOverrideForChat(chatID)
	if strings.TrimSpace(model) != "" {
		return model
	}
	return defaultString(a.cfg.CodexModel, "gpt-5.3-codex-spark")
}

func (a *relayApp) modelSummaryForChat(chatID int64) string {
	model := a.modelForChat(chatID)
	if strings.TrimSpace(a.modelOverrideForChat(chatID)) == "" {
		return fmt.Sprintf("%s (default)", model)
	}
	return fmt.Sprintf("%s (override)", model)
}

func enabledDisabled(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func defaultString(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func firstCommandToken(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	token := text
	if index := strings.IndexAny(token, " \t\r\n"); index >= 0 {
		token = token[:index]
	}
	if index := strings.Index(token, "@"); index >= 0 {
		token = token[:index]
	}
	return token
}

func formatTokenUsage(usage *codex.TokenUsage) string {
	if usage == nil {
		return "(not available yet)"
	}
	parts := []string{}
	if usage.Input > 0 || usage.Output > 0 {
		parts = append(parts, fmt.Sprintf("input=%d", usage.Input))
		parts = append(parts, fmt.Sprintf("output=%d", usage.Output))
	}
	if usage.Total > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("total=%d", usage.Total))
	}
	return strings.Join(parts, " ")
}
