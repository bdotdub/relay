package relay

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func (a *relayApp) modelOverrideForChat(chatID int64) string {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.modelByChat[chatID]
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
	command := strings.TrimSpace(text)
	arg := ""
	if index := strings.IndexAny(command, " \t\r\n"); index >= 0 {
		arg = strings.TrimSpace(command[index+1:])
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
	logx.Debugf("chat verbose mode changed %s", logx.KVSummary("chat_id", chatID, "enabled", a.verboseByChat[chatID]))

	if a.verboseByChat[chatID] {
		return true, ""
	}
	return false, ""
}

func (a *relayApp) toggleYoloForChat(chatID int64, text string) (bool, bool, string) {
	command := strings.TrimSpace(text)
	arg := ""
	if index := strings.IndexAny(command, " \t\r\n"); index >= 0 {
		arg = strings.TrimSpace(command[index+1:])
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

	logx.Debugf("chat yolo mode changed %s", logx.KVSummary("chat_id", chatID, "enabled", next))
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
	command := strings.TrimSpace(text)
	arg := ""
	if index := strings.IndexAny(command, " \t\r\n"); index >= 0 {
		arg = strings.TrimSpace(command[index+1:])
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

	logx.Debugf("chat model changed %s", logx.KVSummary("chat_id", chatID, "model", defaultString(nextOverride, a.cfg.CodexModel)))
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
	if err := os.MkdirAll(filepath.Dir(a.cfg.StatePath), 0o755); err != nil {
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
	if err := os.WriteFile(a.cfg.StatePath, data, 0o644); err != nil {
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
		var chatID int64
		if _, err := fmt.Sscanf(rawChatID, "%d", &chatID); err != nil {
			continue
		}
		decoded[chatID] = value
	}
	return decoded
}
