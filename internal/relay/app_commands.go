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
		return w.app.telegram.SendMessage(ctx, w.chatID, fmt.Sprintf("Transport: %s\nExecution: %s\nFast mode: %s\nModel: %s\nThread: %s\nCWD: %s\nTokens: %s", mode, w.app.executionProfileSummary(w.chatID), enabledDisabled(w.app.fastModeForChat(w.chatID)), w.app.modelSummaryForChat(w.chatID), threadID, w.app.cfg.CodexCWD, formatTokenUsage(w.app.lastUsageForChat(w.chatID))))
	case "/help":
		return w.app.telegram.SendMessage(ctx, w.chatID, "Send any text message to relay it to Codex.\n/new or /reset starts a fresh Codex thread.\n/status shows the current thread mapping, execution mode, fast-mode state, model, and last token usage.\n/verbose toggles intermediate visible output for this chat.\n/yolo toggles YOLO execution mode for this chat and starts a fresh thread.\n/fast toggles fast mode for this chat and starts a fresh thread.\n/model sets a per-chat model override and starts a fresh thread.\n/reload replaces the running relay process with the current binary.")
	case "/reload":
		if err := w.app.telegram.SendMessage(ctx, w.chatID, "Reloading the relay process from the current binary. Active turns will be interrupted."); err != nil {
			return err
		}
		if err := w.app.reload(); err != nil {
			return fmt.Errorf("reload relay process: %w", err)
		}
		return nil
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
		threadID, err := w.app.codex.NewThread(ctx, w.app.threadOptionsForChat(w.chatID))
		if err != nil {
			return err
		}
		if err := w.app.setThreadIDForChat(w.chatID, threadID); err != nil {
			return err
		}
		return w.app.telegram.SendMessage(ctx, w.chatID, fmt.Sprintf("YOLO mode %s for this chat. Started a fresh Codex thread with model %s.\nthread_id=%s", enabledDisabled(enabled), w.app.modelForChat(w.chatID), threadID))
	case "/fast":
		enabled, changed, message := w.app.toggleFastModeForChat(w.chatID, text)
		if message != "" {
			return w.app.telegram.SendMessage(ctx, w.chatID, message)
		}
		if !changed {
			return w.app.telegram.SendMessage(ctx, w.chatID, fmt.Sprintf("Fast mode is already %s for this chat.", enabledDisabled(enabled)))
		}
		threadID, err := w.app.codex.NewThread(ctx, w.app.threadOptionsForChat(w.chatID))
		if err != nil {
			return err
		}
		if err := w.app.setThreadIDForChat(w.chatID, threadID); err != nil {
			return err
		}
		return w.app.telegram.SendMessage(ctx, w.chatID, fmt.Sprintf("Fast mode %s for this chat. Started a fresh Codex thread with model %s.\nthread_id=%s", enabledDisabled(enabled), w.app.modelForChat(w.chatID), threadID))
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
	options := codex.ThreadOptions{
		Yolo:  a.yoloForChat(chatID),
		Model: a.modelForChat(chatID),
	}
	if tier, ok := a.serviceTierOverrideForChat(chatID); ok {
		options.ServiceTierSet = true
		if strings.EqualFold(strings.TrimSpace(tier), "fast") {
			options.ServiceTier = "fast"
		}
	}
	return options
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
	return defaultString(a.cfg.CodexModel, "gpt-5.4")
}

func (a *relayApp) modelSummaryForChat(chatID int64) string {
	model := a.modelForChat(chatID)
	if strings.TrimSpace(a.modelOverrideForChat(chatID)) == "" {
		return fmt.Sprintf("%s (default)", model)
	}
	return fmt.Sprintf("%s (override)", model)
}

func (a *relayApp) fastModeForChat(chatID int64) bool {
	if tier, ok := a.serviceTierOverrideForChat(chatID); ok {
		return strings.EqualFold(strings.TrimSpace(tier), "fast")
	}
	return strings.EqualFold(strings.TrimSpace(a.cfg.CodexServiceTier), "fast")
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
