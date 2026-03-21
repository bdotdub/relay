package relay

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const continuityRecentMessageLimit = 20

func (a *relayApp) continuityForChat(chatID int64) chatContinuityState {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return cloneChatContinuityState(a.continuityByChat[chatID])
}

func (a *relayApp) recordUserTurnStart(chatID int64, text string) error {
	return a.updateContinuityForChat(chatID, func(state *chatContinuityState) {
		appendRecentMessage(state, "user", continuityMessageText(text))
		state.PendingTurn = &pendingTurnSnapshot{
			StartedAt:       time.Now().UTC().Format(time.RFC3339),
			LastUserMessage: continuityMessageText(text),
		}
	})
}

func (a *relayApp) recordAssistantTurnCompletion(chatID int64, text string) error {
	return a.updateContinuityForChat(chatID, func(state *chatContinuityState) {
		appendRecentMessage(state, "assistant", continuityMessageText(text))
		state.PendingTurn = nil
	})
}

func (a *relayApp) clearPendingTurn(chatID int64) error {
	return a.updateContinuityForChat(chatID, func(state *chatContinuityState) {
		state.PendingTurn = nil
	})
}

func (a *relayApp) resetContinuityForChat(chatID int64) error {
	return a.updateContinuityForChat(chatID, func(state *chatContinuityState) {
		state.RecentMessages = nil
		state.PendingTurn = nil
	})
}

func (a *relayApp) updateContinuityForChat(chatID int64, mutate func(*chatContinuityState)) error {
	a.stateMu.Lock()
	state := cloneChatContinuityState(a.continuityByChat[chatID])
	mutate(&state)
	if len(state.RecentMessages) == 0 && state.PendingTurn == nil {
		delete(a.continuityByChat, chatID)
	} else {
		a.continuityByChat[chatID] = state
	}
	a.stateMu.Unlock()
	return a.saveState()
}

func appendRecentMessage(state *chatContinuityState, role string, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	state.RecentMessages = append(state.RecentMessages, relayMessageSnapshot{
		Role:      role,
		Text:      text,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
	if len(state.RecentMessages) > continuityRecentMessageLimit {
		state.RecentMessages = append([]relayMessageSnapshot(nil), state.RecentMessages[len(state.RecentMessages)-continuityRecentMessageLimit:]...)
	}
}

func continuityMessageText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "(no text)"
	}
	return text
}

func buildContinuityBootstrap(state chatContinuityState, latestUserText string, hasImages bool) string {
	var builder strings.Builder
	builder.WriteString("The previous relay process restarted, and the earlier Codex thread could not be resumed. Continue from this local continuity snapshot instead.\n")
	if state.PendingTurn != nil {
		builder.WriteString("\nThe most recent turn may have been interrupted before the user received a reply.\n")
	}
	if len(state.RecentMessages) > 0 {
		builder.WriteString("\nRecent conversation:\n")
		for _, message := range state.RecentMessages {
			role := strings.TrimSpace(message.Role)
			if role != "" {
				role = strings.ToUpper(role[:1]) + role[1:]
			}
			if role == "" {
				role = "Message"
			}
			builder.WriteString(fmt.Sprintf("%s: %s\n", role, message.Text))
		}
	}
	builder.WriteString("\nRespond to the latest user message below.\n")
	if strings.TrimSpace(latestUserText) != "" {
		builder.WriteString("\nLatest user text:\n")
		builder.WriteString(latestUserText)
		builder.WriteString("\n")
	}
	if hasImages {
		builder.WriteString("\nThe latest user message also includes one or more attached images.\n")
	}
	if strings.TrimSpace(latestUserText) == "" && !hasImages {
		builder.WriteString("\nThe latest user message had no text content.\n")
	}
	return strings.TrimSpace(builder.String())
}

func cloneChatContinuityState(state chatContinuityState) chatContinuityState {
	cloned := chatContinuityState{}
	if len(state.RecentMessages) > 0 {
		cloned.RecentMessages = append([]relayMessageSnapshot(nil), state.RecentMessages...)
	}
	if state.PendingTurn != nil {
		pending := *state.PendingTurn
		cloned.PendingTurn = &pending
	}
	return cloned
}

func encodeContinuityMap(values map[int64]chatContinuityState) map[string]chatContinuityState {
	if len(values) == 0 {
		return nil
	}
	encoded := make(map[string]chatContinuityState, len(values))
	for chatID, state := range values {
		cloned := cloneChatContinuityState(state)
		if len(cloned.RecentMessages) == 0 && cloned.PendingTurn == nil {
			continue
		}
		encoded[fmt.Sprintf("%d", chatID)] = cloned
	}
	if len(encoded) == 0 {
		return nil
	}
	return encoded
}

func decodeContinuityMap(values map[string]chatContinuityState) map[int64]chatContinuityState {
	if len(values) == 0 {
		return map[int64]chatContinuityState{}
	}
	decoded := make(map[int64]chatContinuityState, len(values))
	for rawChatID, state := range values {
		chatID, err := strconv.ParseInt(strings.TrimSpace(rawChatID), 10, 64)
		if err != nil {
			continue
		}
		decoded[chatID] = cloneChatContinuityState(state)
	}
	return decoded
}
