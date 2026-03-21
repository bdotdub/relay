package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bdotdub/relay/internal/codex"
	"github.com/bdotdub/relay/internal/logx"
)

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

func (a *relayApp) serviceTierOverrideForChat(chatID int64) (string, bool) {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	value, ok := a.serviceTierByChat[chatID]
	return value, ok
}

func (a *relayApp) modelOverrideForChat(chatID int64) string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.modelByChat[chatID]
}

func (a *relayApp) reasoningEffortOverrideForChat(chatID int64) string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.reasoningEffortByChat[chatID]
}

func (a *relayApp) lastUsageForChat(chatID int64) *codex.TokenUsage {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	usage, ok := a.lastUsageByChat[chatID]
	if !ok {
		return nil
	}
	copy := usage
	return &copy
}

func (a *relayApp) toggleVerboseForChat(chatID int64, text string) (bool, string) {
	arg := commandArgument(text)

	a.stateMu.Lock()
	current := a.verboseByChat[chatID]
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
		if current {
			return true, "Verbose mode is enabled for this chat."
		}
		return false, "Verbose mode is disabled for this chat."
	default:
		a.stateMu.Unlock()
		return current, "Usage: /verbose, /verbose on, /verbose off, or /verbose status"
	}

	if next == current {
		a.stateMu.Unlock()
		if current {
			return true, ""
		}
		return false, ""
	}
	a.verboseByChat[chatID] = next
	a.stateMu.Unlock()

	logx.Debug("chat verbose mode changed", "chat_id", chatID, "enabled", next)
	if err := a.saveState(); err != nil {
		a.stateMu.Lock()
		if current {
			a.verboseByChat[chatID] = true
		} else {
			delete(a.verboseByChat, chatID)
		}
		a.stateMu.Unlock()
		return current, fmt.Sprintf("Failed to update verbose mode: %v", err)
	}

	if next {
		return true, ""
	}
	return false, ""
}

func (a *relayApp) toggleYoloForChat(chatID int64, text string) (bool, bool, string) {
	arg := commandArgument(text)

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

	logx.Debug("chat yolo mode changed", "chat_id", chatID, "enabled", next)
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
	arg := commandArgument(text)
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

	logx.Debug("chat model changed", "chat_id", chatID, "model", defaultString(nextOverride, a.cfg.CodexModel))
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

func (a *relayApp) setReasoningEffortForChat(chatID int64, text string) (string, bool, string) {
	arg := commandArgument(text)
	if arg == "" || arg == "status" {
		summary := a.reasoningEffortSummaryForChat(chatID)
		return summary, false, fmt.Sprintf("Reasoning is %s for this chat. Choices: %s.", summary, supportedReasoningEffortsList)
	}

	nextOverride := strings.ToLower(strings.TrimSpace(arg))
	switch nextOverride {
	case "default", "reset", "clear":
		nextOverride = ""
	default:
		if !isSupportedReasoningEffort(nextOverride) {
			return a.reasoningEffortSummaryForChat(chatID), false, fmt.Sprintf("Usage: /reasoning <level>. Supported levels: %s. Use /reasoning default to clear the override.", supportedReasoningEffortsList)
		}
	}

	a.stateMu.Lock()
	currentOverride := a.reasoningEffortByChat[chatID]
	if currentOverride == nextOverride {
		a.stateMu.Unlock()
		return a.reasoningEffortSummaryForChat(chatID), false, ""
	}
	if nextOverride == "" {
		delete(a.reasoningEffortByChat, chatID)
	} else {
		a.reasoningEffortByChat[chatID] = nextOverride
	}
	a.stateMu.Unlock()

	logx.Debug("chat reasoning effort changed", "chat_id", chatID, "reasoning_effort", defaultString(nextOverride, "default"))
	if err := a.saveState(); err != nil {
		a.stateMu.Lock()
		if currentOverride == "" {
			delete(a.reasoningEffortByChat, chatID)
		} else {
			a.reasoningEffortByChat[chatID] = currentOverride
		}
		a.stateMu.Unlock()
		return a.reasoningEffortSummaryForChat(chatID), false, fmt.Sprintf("Failed to update reasoning effort: %v", err)
	}
	return a.reasoningEffortForChat(chatID), true, ""
}

func (a *relayApp) toggleFastModeForChat(chatID int64, text string) (bool, bool, string) {
	arg := commandArgument(text)

	current := a.fastModeForChat(chatID)
	next := current
	switch arg {
	case "", "toggle":
		next = !current
	case "on":
		next = true
	case "off":
		next = false
	case "status":
		return current, false, fmt.Sprintf("Fast mode is %s for this chat.", enabledDisabled(current))
	default:
		return current, false, "Usage: /fast, /fast on, /fast off, or /fast status"
	}

	if next == current {
		return current, false, ""
	}

	a.stateMu.Lock()
	if next {
		a.serviceTierByChat[chatID] = "fast"
	} else {
		a.serviceTierByChat[chatID] = "default"
	}
	a.stateMu.Unlock()

	logx.Debug("chat fast mode changed", "chat_id", chatID, "enabled", next)
	if err := a.saveState(); err != nil {
		a.stateMu.Lock()
		if current {
			a.serviceTierByChat[chatID] = "fast"
		} else {
			a.serviceTierByChat[chatID] = "default"
		}
		a.stateMu.Unlock()
		return current, false, fmt.Sprintf("Failed to update fast mode: %v", err)
	}
	return next, true, ""
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

func (a *relayApp) setLastUsageForChat(chatID int64, usage *codex.TokenUsage) {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	if usage == nil {
		delete(a.lastUsageByChat, chatID)
		return
	}
	a.lastUsageByChat[chatID] = *usage
}

func (a *relayApp) loadState() error {
	data, err := os.ReadFile(a.cfg.StatePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			a.stateMu.Lock()
			a.resetStateLocked()
			a.stateMu.Unlock()
			return nil
		}
		return fmt.Errorf("read relay state: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err == nil {
		if _, hasThreads := raw["threads"]; hasThreads || raw["verbose_by_chat"] != nil || raw["yolo_by_chat"] != nil || raw["service_tier_by_chat"] != nil || raw["model_by_chat"] != nil || raw["reasoning_effort_by_chat"] != nil || raw["continuity_by_chat"] != nil {
			var state relayState
			if err := json.Unmarshal(data, &state); err != nil {
				a.stateMu.Lock()
				a.resetStateLocked()
				a.stateMu.Unlock()
				return nil
			}
			a.stateMu.Lock()
			a.threadIDsByChat = state.Threads
			if a.threadIDsByChat == nil {
				a.threadIDsByChat = map[string]string{}
			}
			a.verboseByChat = decodeBoolMap(state.VerboseByChat)
			a.yoloByChat = decodeBoolMap(state.YoloByChat)
			a.serviceTierByChat = decodeStringMap(state.ServiceTierByChat)
			a.modelByChat = decodeStringMap(state.ModelByChat)
			a.reasoningEffortByChat = decodeStringMap(state.ReasoningEffortByChat)
			a.continuityByChat = decodeContinuityMap(state.ContinuityByChat)
			a.stateMu.Unlock()
			return nil
		}
	}

	var mapping map[string]string
	if err := json.Unmarshal(data, &mapping); err != nil {
		a.stateMu.Lock()
		a.resetStateLocked()
		a.stateMu.Unlock()
		return nil
	}
	a.stateMu.Lock()
	a.resetStateLocked()
	a.threadIDsByChat = mapping
	a.stateMu.Unlock()
	return nil
}

func (a *relayApp) saveState() error {
	stateDir := filepath.Dir(a.cfg.StatePath)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return fmt.Errorf("chmod state directory: %w", err)
	}

	a.stateMu.RLock()
	state := relayState{
		Threads:               a.threadIDsByChat,
		VerboseByChat:         encodeBoolMap(a.verboseByChat),
		YoloByChat:            encodeBoolMap(a.yoloByChat),
		ServiceTierByChat:     encodeStringMap(a.serviceTierByChat),
		ModelByChat:           encodeStringMap(a.modelByChat),
		ReasoningEffortByChat: encodeStringMap(a.reasoningEffortByChat),
		ContinuityByChat:      encodeContinuityMap(a.continuityByChat),
	}
	data, err := json.MarshalIndent(state, "", "  ")
	a.stateMu.RUnlock()
	if err != nil {
		return fmt.Errorf("marshal relay state: %w", err)
	}
	data = append(data, '\n')
	tempFile, err := os.CreateTemp(stateDir, filepath.Base(a.cfg.StatePath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tempPath := tempFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()
	if err := tempFile.Chmod(0o600); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("chmod temp state file: %w", err)
	}
	if _, err := tempFile.Write(data); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := os.Rename(tempPath, a.cfg.StatePath); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	cleanup = false
	if err := os.Chmod(a.cfg.StatePath, 0o600); err != nil {
		return fmt.Errorf("chmod state file: %w", err)
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
		chatID, err := strconv.ParseInt(strings.TrimSpace(rawChatID), 10, 64)
		if err != nil {
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
		value = strings.TrimSpace(value)
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
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		chatID, err := strconv.ParseInt(strings.TrimSpace(rawChatID), 10, 64)
		if err != nil {
			continue
		}
		decoded[chatID] = value
	}
	return decoded
}

func (a *relayApp) reasoningEffortForChat(chatID int64) string {
	value := a.reasoningEffortOverrideForChat(chatID)
	if strings.TrimSpace(value) == "" {
		return "default"
	}
	return value
}

func (a *relayApp) reasoningEffortSummaryForChat(chatID int64) string {
	value := strings.TrimSpace(a.reasoningEffortOverrideForChat(chatID))
	if value == "" {
		return "default (model default)"
	}
	return fmt.Sprintf("%s (override)", value)
}

func isSupportedReasoningEffort(value string) bool {
	for _, effort := range supportedReasoningEfforts {
		if value == effort {
			return true
		}
	}
	return false
}

func (a *relayApp) resetStateLocked() {
	a.threadIDsByChat = map[string]string{}
	a.verboseByChat = map[int64]bool{}
	a.yoloByChat = map[int64]bool{}
	a.serviceTierByChat = map[int64]string{}
	a.modelByChat = map[int64]string{}
	a.reasoningEffortByChat = map[int64]string{}
	a.continuityByChat = map[int64]chatContinuityState{}
}

var supportedReasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

const supportedReasoningEffortsList = "none, minimal, low, medium, high, xhigh"
