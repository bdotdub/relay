package relay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	codexpkg "github.com/bdotdub/relay/internal/codex"
	configpkg "github.com/bdotdub/relay/internal/config"
	telegrampkg "github.com/bdotdub/relay/internal/telegram"
)

type telegramUpdate = telegrampkg.Update
type telegramMessage = telegrampkg.Message
type telegramChat = telegrampkg.Chat
type turnResult = codexpkg.TurnResult
type turnStreamEvent = codexpkg.TurnStreamEvent
type tokenUsage = codexpkg.TokenUsage
type codexTurnHandle = codexpkg.TurnHandle
type codexThreadOptions = codexpkg.ThreadOptions

var stringsContains = strings.Contains

func privateChat(id int64) telegramChat {
	return telegramChat{ID: id, Type: "private"}
}

func TestSameChatSteersActiveTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	firstMessage := telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 101,
			Chat:      privateChat(7),
			Text:      "Write a summary",
		},
	}
	secondMessage := telegramUpdate{
		UpdateID: 2,
		Message: &telegramMessage{
			MessageID: 102,
			Chat:      privateChat(7),
			Text:      "Include edge cases too",
		},
	}

	if err := app.handleUpdate(ctx, firstMessage); err != nil {
		t.Fatalf("handle first update: %v", err)
	}
	waitFor(t, "first turn start", func() bool {
		return codex.startCount() == 1
	})

	if err := app.handleUpdate(ctx, secondMessage); err != nil {
		t.Fatalf("handle second update: %v", err)
	}
	waitFor(t, "turn steer", func() bool {
		return codex.steerCount() == 1
	})

	codex.finishTurn("thread-1", turnResult{Text: "Final answer"})

	waitFor(t, "telegram reply", func() bool {
		return telegram.messageCount() == 1
	})

	message := telegram.messages()[0]
	if message.text != "Final answer" {
		t.Fatalf("unexpected reply text: %q", message.text)
	}
}

func TestRunRetriesTransientTelegramGetUpdatesErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	telegram.queueGetUpdatesResponse(nil, &telegrampkg.RequestError{
		Method:     "getUpdates",
		StatusCode: 502,
		Status:     "502 Bad Gateway",
	})
	telegram.queueGetUpdatesResponse([]telegramUpdate{
		{
			UpdateID: 1,
			Message: &telegramMessage{
				MessageID: 1001,
				Chat:      privateChat(7),
				Text:      "Still there?",
			},
		},
	}, nil)

	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)
	app.sleep = func(context.Context, time.Duration) error { return nil }

	done := make(chan error, 1)
	go func() {
		done <- app.run(ctx)
	}()

	waitFor(t, "retry reaches next poll", func() bool {
		return telegram.getUpdatesCount() >= 2
	})
	waitFor(t, "turn start after retry", func() bool {
		return codex.startCount() == 1
	})

	codex.finishTurn("thread-1", turnResult{Text: "Yep"})
	waitFor(t, "telegram reply after retry", func() bool {
		return telegram.messageCount() == 1
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error after cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for run to stop")
	}
}

func TestReplyContextPassedToCodexOnStartTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 10,
		Message: &telegramMessage{
			MessageID: 110,
			Chat:      privateChat(7),
			Text:      "Do that version",
			ReplyToMessage: &telegramMessage{
				MessageID: 109,
				Text:      "Please refactor the parser and keep tests green.",
			},
		},
	}); err != nil {
		t.Fatalf("handle update: %v", err)
	}

	waitFor(t, "reply-context turn start", func() bool {
		return codex.startCount() == 1
	})

	call := codex.startCallsSnapshot()[0]
	if !stringsContains(call.text, "The user is replying to a specific earlier Telegram message.") {
		t.Fatalf("expected reply context preamble, got %q", call.text)
	}
	if !stringsContains(call.text, "Replied-to message:\nPlease refactor the parser and keep tests green.") {
		t.Fatalf("expected replied-to message text, got %q", call.text)
	}
	if !stringsContains(call.text, "Latest user message:\nDo that version") {
		t.Fatalf("expected latest user text in contextualized prompt, got %q", call.text)
	}
}

func TestReplyContextPassedToCodexOnSteerTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 11,
		Message: &telegramMessage{
			MessageID: 111,
			Chat:      privateChat(7),
			Text:      "Initial request",
		},
	}); err != nil {
		t.Fatalf("handle first update: %v", err)
	}
	waitFor(t, "initial start", func() bool {
		return codex.startCount() == 1
	})

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 12,
		Message: &telegramMessage{
			MessageID: 112,
			Chat:      privateChat(7),
			Text:      "Use this one instead",
			ReplyToMessage: &telegramMessage{
				MessageID: 111,
				Text:      "Initial request",
				Photo: []telegrampkg.PhotoSize{
					{FileID: "reply-photo", Width: 200, Height: 200},
				},
			},
		},
	}); err != nil {
		t.Fatalf("handle second update: %v", err)
	}
	waitFor(t, "reply-context steer", func() bool {
		return codex.steerCount() == 1
	})

	call := codex.steerCallsSnapshot()[0]
	if !stringsContains(call.text, "Replied-to message:\nInitial request") {
		t.Fatalf("expected replied-to steer text, got %q", call.text)
	}
	if !stringsContains(call.text, "The replied-to message also included one or more attached images.") {
		t.Fatalf("expected replied-to image note, got %q", call.text)
	}
	if !stringsContains(call.text, "Latest user message:\nUse this one instead") {
		t.Fatalf("expected latest steer text, got %q", call.text)
	}
}

func TestContextWindowExceededCompactsAndRetriesTurn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 3,
		Message: &telegramMessage{
			MessageID: 103,
			Chat:      privateChat(7),
			Text:      "Recover this request",
		},
	}); err != nil {
		t.Fatalf("handle update: %v", err)
	}

	waitFor(t, "initial turn start", func() bool {
		return codex.startCount() == 1
	})

	codex.finishTurn("thread-1", turnResult{
		ErrorMessage: "context window exceeded",
		ErrorCode:    codexpkg.TurnErrorCodeContextWindowExceeded,
	})

	waitFor(t, "compaction retry", func() bool {
		return codex.startCount() == 2
	})

	compactCalls := codex.compactCallsSnapshot()
	if len(compactCalls) != 1 || compactCalls[0] != "thread-1" {
		t.Fatalf("unexpected compact calls: %#v", compactCalls)
	}

	codex.finishTurn("thread-1", turnResult{Text: "Recovered answer"})

	waitFor(t, "telegram reply after retry", func() bool {
		return telegram.messageCount() == 1
	})

	message := telegram.messages()[0]
	if message.text != "Recovered answer" {
		t.Fatalf("unexpected reply text after retry: %q", message.text)
	}
}

func TestContextWindowExceededReplaysSteeredInputsAfterCompaction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 4,
		Message: &telegramMessage{
			MessageID: 104,
			Chat:      privateChat(7),
			Text:      "First request",
		},
	}); err != nil {
		t.Fatalf("handle first update: %v", err)
	}
	waitFor(t, "first turn start", func() bool {
		return codex.startCount() == 1
	})

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 5,
		Message: &telegramMessage{
			MessageID: 105,
			Chat:      privateChat(7),
			Text:      "Second request",
		},
	}); err != nil {
		t.Fatalf("handle second update: %v", err)
	}
	waitFor(t, "initial steer", func() bool {
		return codex.steerCount() == 1
	})

	codex.finishTurn("thread-1", turnResult{
		ErrorMessage: "context window exceeded",
		ErrorCode:    codexpkg.TurnErrorCodeContextWindowExceeded,
	})

	waitFor(t, "retry start and steer", func() bool {
		return codex.startCount() == 2 && codex.steerCount() == 2
	})

	startCalls := codex.startCallsSnapshot()
	if len(startCalls) != 2 {
		t.Fatalf("unexpected start calls: %#v", startCalls)
	}
	if startCalls[1].text != "First request" {
		t.Fatalf("expected first input to be replayed, got %q", startCalls[1].text)
	}

	steerCalls := codex.steerCallsSnapshot()
	if len(steerCalls) != 2 {
		t.Fatalf("unexpected steer calls: %#v", steerCalls)
	}
	if steerCalls[1].text != "Second request" {
		t.Fatalf("expected second input to be replayed, got %q", steerCalls[1].text)
	}

	codex.finishTurn("thread-1", turnResult{Text: "Recovered with both inputs"})

	waitFor(t, "telegram reply after replay", func() bool {
		return telegram.messageCount() == 1
	})

	if got := telegram.messages()[0].text; got != "Recovered with both inputs" {
		t.Fatalf("unexpected reply text: %q", got)
	}
}

func TestContextWindowExceededRetriesOnlyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 6,
		Message: &telegramMessage{
			MessageID: 106,
			Chat:      privateChat(7),
			Text:      "Still too large",
		},
	}); err != nil {
		t.Fatalf("handle update: %v", err)
	}
	waitFor(t, "initial turn start", func() bool {
		return codex.startCount() == 1
	})

	codex.finishTurn("thread-1", turnResult{
		ErrorMessage: "context window exceeded",
		ErrorCode:    codexpkg.TurnErrorCodeContextWindowExceeded,
	})
	waitFor(t, "retry start", func() bool {
		return codex.startCount() == 2
	})

	codex.finishTurn("thread-1", turnResult{
		ErrorMessage: "context window exceeded",
		ErrorCode:    codexpkg.TurnErrorCodeContextWindowExceeded,
	})
	waitFor(t, "final error reply", func() bool {
		return telegram.messageCount() == 1
	})

	if codex.startCount() != 2 {
		t.Fatalf("expected exactly one retry, got %d starts", codex.startCount())
	}
	compactCalls := codex.compactCallsSnapshot()
	if len(compactCalls) != 1 {
		t.Fatalf("expected exactly one compact call, got %#v", compactCalls)
	}
	if !stringsContains(telegram.messages()[0].text, "Codex reported an error: context window exceeded") {
		t.Fatalf("unexpected error reply: %q", telegram.messages()[0].text)
	}
}

func TestDifferentChatsStartDifferentTurns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	for _, update := range []telegramUpdate{
		{
			UpdateID: 1,
			Message: &telegramMessage{
				MessageID: 201,
				Chat:      privateChat(11),
				Text:      "Chat one",
			},
		},
		{
			UpdateID: 2,
			Message: &telegramMessage{
				MessageID: 301,
				Chat:      privateChat(12),
				Text:      "Chat two",
			},
		},
	} {
		if err := app.handleUpdate(ctx, update); err != nil {
			t.Fatalf("handle update %d: %v", update.UpdateID, err)
		}
	}

	waitFor(t, "two started turns", func() bool {
		return codex.startCount() == 2
	})

	if codex.steerCount() != 0 {
		t.Fatalf("expected no steering across chats, got %d steer calls", codex.steerCount())
	}
}

func TestVerboseShowsIntermediateSections(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 401,
			Chat:      privateChat(21),
			Text:      "/verbose on",
		},
	}); err != nil {
		t.Fatalf("handle verbose command: %v", err)
	}

	waitFor(t, "verbose ack", func() bool {
		return telegram.messageCount() == 1
	})

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 2,
		Message: &telegramMessage{
			MessageID: 402,
			Chat:      privateChat(21),
			Text:      "Do the thing",
		},
	}); err != nil {
		t.Fatalf("handle message: %v", err)
	}

	waitFor(t, "turn start", func() bool {
		return codex.startCount() == 1
	})

	codex.emitTurnEvent("thread-1", turnStreamEvent{Text: "Working on it"})

	waitFor(t, "intermediate reply", func() bool {
		return telegram.messageCount() == 2
	})

	codex.finishTurn("thread-1", turnResult{
		Text:             "Final answer",
		CommentaryText:   "Working on it",
		PlanText:         "1. Inspect\n2. Change",
		ReasoningSummary: "Used the existing structure and simplified the flow.",
	})

	waitFor(t, "verbose reply", func() bool {
		return telegram.messageCount() == 3
	})

	messages := telegram.messages()
	if messages[1].text != "Working on it" {
		t.Fatalf("unexpected intermediate reply: %q", messages[1].text)
	}
	if messages[2].text != "Final answer" {
		t.Fatalf("unexpected final reply: %q", messages[2].text)
	}
}

func TestIntermediateUpdatesAreMutedWhenVerboseIsOff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 451,
			Chat:      privateChat(22),
			Text:      "Do the thing",
		},
	}); err != nil {
		t.Fatalf("handle message: %v", err)
	}

	waitFor(t, "turn start", func() bool {
		return codex.startCount() == 1
	})

	codex.emitTurnEvent("thread-1", turnStreamEvent{Text: "Working on it"})
	time.Sleep(50 * time.Millisecond)

	if telegram.messageCount() != 0 {
		t.Fatalf("expected no intermediate reply while verbose is off, got %d", telegram.messageCount())
	}

	codex.finishTurn("thread-1", turnResult{Text: "Final answer"})

	waitFor(t, "final reply", func() bool {
		return telegram.messageCount() == 1
	})
}

func TestStatusShowsLastTokenUsage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 501,
			Chat:      privateChat(31),
			Text:      "Count tokens",
		},
	}); err != nil {
		t.Fatalf("handle message: %v", err)
	}

	waitFor(t, "turn start", func() bool {
		return codex.startCount() == 1
	})

	codex.finishTurn("thread-1", turnResult{
		Text:  "Done",
		Usage: &tokenUsage{Input: 120, Output: 34, Total: 154},
	})

	waitFor(t, "turn reply", func() bool {
		return telegram.messageCount() == 1
	})

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 2,
		Message: &telegramMessage{
			MessageID: 502,
			Chat:      privateChat(31),
			Text:      "/status",
		},
	}); err != nil {
		t.Fatalf("handle status command: %v", err)
	}

	waitFor(t, "status reply", func() bool {
		return telegram.messageCount() == 2
	})

	reply := telegram.messages()[1].text
	if !stringsContains(reply, "Tokens: input=120 output=34 total=154") {
		t.Fatalf("status reply missing token usage: %q", reply)
	}
	if !stringsContains(reply, "Execution: default (approval=never, sandbox=workspace-write)") {
		t.Fatalf("status reply missing execution profile: %q", reply)
	}
	if !stringsContains(reply, "Fast mode: enabled") {
		t.Fatalf("status reply missing fast mode: %q", reply)
	}
	if !stringsContains(reply, "Model: gpt-5.4 (default)") {
		t.Fatalf("status reply missing default model: %q", reply)
	}
}

func TestYoloCommandStartsFreshThreadAndUpdatesStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 551,
			Chat:      privateChat(32),
			Text:      "/yolo on",
		},
	}); err != nil {
		t.Fatalf("handle yolo command: %v", err)
	}

	waitFor(t, "yolo ack", func() bool {
		return telegram.messageCount() == 1
	})

	reply := telegram.messages()[0].text
	if !stringsContains(reply, "YOLO mode enabled") || !stringsContains(reply, "thread_id=new-thread-1") {
		t.Fatalf("unexpected yolo ack: %q", reply)
	}

	newCalls := codex.newThreadCallsSnapshot()
	if len(newCalls) != 1 || !newCalls[0].options.Yolo {
		t.Fatalf("expected yolo new thread call, got %#v", newCalls)
	}

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 2,
		Message: &telegramMessage{
			MessageID: 552,
			Chat:      privateChat(32),
			Text:      "/status",
		},
	}); err != nil {
		t.Fatalf("handle status command: %v", err)
	}

	waitFor(t, "status reply", func() bool {
		return telegram.messageCount() == 2
	})

	status := telegram.messages()[1].text
	if !stringsContains(status, "Execution: YOLO (approval=never, sandbox=danger-full-access)") {
		t.Fatalf("status reply missing yolo profile: %q", status)
	}
	if !stringsContains(status, "Fast mode: enabled") {
		t.Fatalf("status reply missing fast mode: %q", status)
	}
	if !stringsContains(status, "Model: gpt-5.4 (default)") {
		t.Fatalf("status reply missing default model: %q", status)
	}
	if !stringsContains(status, "Thread: new-thread-1") {
		t.Fatalf("status reply missing yolo thread id: %q", status)
	}
}

func TestFastCommandStartsFreshThreadAndUpdatesStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 553,
			Chat:      privateChat(32),
			Text:      "/fast off",
		},
	}); err != nil {
		t.Fatalf("handle fast command: %v", err)
	}

	waitFor(t, "fast ack", func() bool {
		return telegram.messageCount() == 1
	})

	reply := telegram.messages()[0].text
	if !stringsContains(reply, "Fast mode disabled") || !stringsContains(reply, "thread_id=new-thread-1") {
		t.Fatalf("unexpected fast ack: %q", reply)
	}

	newCalls := codex.newThreadCallsSnapshot()
	if len(newCalls) != 1 || !newCalls[0].options.ServiceTierSet || newCalls[0].options.ServiceTier != "" {
		t.Fatalf("expected fast override to clear service tier, got %#v", newCalls)
	}

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 2,
		Message: &telegramMessage{
			MessageID: 554,
			Chat:      privateChat(32),
			Text:      "/status",
		},
	}); err != nil {
		t.Fatalf("handle status command: %v", err)
	}

	waitFor(t, "fast status reply", func() bool {
		return telegram.messageCount() == 2
	})

	status := telegram.messages()[1].text
	if !stringsContains(status, "Fast mode: disabled") {
		t.Fatalf("status reply missing fast mode override: %q", status)
	}
}

func TestModelCommandStartsFreshThreadAndUpdatesStatus(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 556,
			Chat:      privateChat(34),
			Text:      "/model gpt-5",
		},
	}); err != nil {
		t.Fatalf("handle model command: %v", err)
	}

	waitFor(t, "model ack", func() bool {
		return telegram.messageCount() == 1
	})

	reply := telegram.messages()[0].text
	if !stringsContains(reply, "Model set to gpt-5") || !stringsContains(reply, "thread_id=new-thread-1") {
		t.Fatalf("unexpected model ack: %q", reply)
	}

	newCalls := codex.newThreadCallsSnapshot()
	if len(newCalls) != 1 || newCalls[0].options.Model != "gpt-5" {
		t.Fatalf("expected model override new thread call, got %#v", newCalls)
	}

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 2,
		Message: &telegramMessage{
			MessageID: 557,
			Chat:      privateChat(34),
			Text:      "/status",
		},
	}); err != nil {
		t.Fatalf("handle status command: %v", err)
	}

	waitFor(t, "status reply", func() bool {
		return telegram.messageCount() == 2
	})

	status := telegram.messages()[1].text
	if !stringsContains(status, "Model: gpt-5 (override)") {
		t.Fatalf("status reply missing model override: %q", status)
	}
	if !stringsContains(status, "Thread: new-thread-1") {
		t.Fatalf("status reply missing model thread id: %q", status)
	}
}

func TestModelDefaultClearsOverride(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	for _, update := range []telegramUpdate{
		{
			UpdateID: 1,
			Message: &telegramMessage{
				MessageID: 566,
				Chat:      privateChat(35),
				Text:      "/model gpt-5",
			},
		},
		{
			UpdateID: 2,
			Message: &telegramMessage{
				MessageID: 567,
				Chat:      privateChat(35),
				Text:      "/model default",
			},
		},
	} {
		if err := app.handleUpdate(ctx, update); err != nil {
			t.Fatalf("handle update %d: %v", update.UpdateID, err)
		}
	}

	waitFor(t, "model reset ack", func() bool {
		return telegram.messageCount() == 2
	})

	newCalls := codex.newThreadCallsSnapshot()
	if len(newCalls) != 2 {
		t.Fatalf("expected two fresh threads, got %#v", newCalls)
	}
	if newCalls[1].options.Model != "gpt-5.4" {
		t.Fatalf("expected reset to default model, got %#v", newCalls[1])
	}
	if app.modelOverrideForChat(35) != "" {
		t.Fatalf("expected model override to be cleared, got %q", app.modelOverrideForChat(35))
	}
}

func TestFastModeChangesThreadOptionsForSubsequentTurns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 563,
			Chat:      privateChat(33),
			Text:      "/fast off",
		},
	}); err != nil {
		t.Fatalf("handle fast command: %v", err)
	}

	waitFor(t, "fast off ack", func() bool {
		return telegram.messageCount() == 1
	})

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 2,
		Message: &telegramMessage{
			MessageID: 564,
			Chat:      privateChat(33),
			Text:      "Ship it",
		},
	}); err != nil {
		t.Fatalf("handle message: %v", err)
	}

	waitFor(t, "turn start", func() bool {
		return codex.startCount() == 1
	})

	ensureCalls := codex.ensureThreadCallsSnapshot()
	if len(ensureCalls) != 1 || ensureCalls[0].threadID != "new-thread-1" || !ensureCalls[0].options.ServiceTierSet || ensureCalls[0].options.ServiceTier != "" {
		t.Fatalf("expected fast override ensureThread call for the fresh thread, got %#v", ensureCalls)
	}

	codex.finishTurn("new-thread-1", turnResult{Text: "Done"})
}

func TestReloadCommandAcknowledgesAndInvokesReloadHook(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	reloadCalls := 0
	app.reload = func() error {
		reloadCalls++
		return nil
	}

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 570,
			Chat:      privateChat(36),
			Text:      "/reload",
		},
	}); err != nil {
		t.Fatalf("handle reload command: %v", err)
	}

	waitFor(t, "reload ack", func() bool {
		return telegram.messageCount() == 1
	})

	if reloadCalls != 1 {
		t.Fatalf("expected reload hook to be called once, got %d", reloadCalls)
	}

	reply := telegram.messages()[0].text
	if !stringsContains(reply, "Reloading the relay process from the current binary.") {
		t.Fatalf("unexpected reload ack: %q", reply)
	}
}

func TestReloadCommandReportsReloadFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	app.reload = func() error {
		return errors.New("exec failed")
	}

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 571,
			Chat:      privateChat(37),
			Text:      "/reload",
		},
	}); err != nil {
		t.Fatalf("handle reload command: %v", err)
	}

	waitFor(t, "reload failure replies", func() bool {
		return telegram.messageCount() == 2
	})

	messages := telegram.messages()
	if !stringsContains(messages[1].text, "Codex relay error: reload relay process: exec failed") {
		t.Fatalf("unexpected reload failure reply: %q", messages[1].text)
	}
}

func TestYoloModeChangesThreadOptionsForSubsequentTurns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 561,
			Chat:      privateChat(33),
			Text:      "/yolo on",
		},
	}); err != nil {
		t.Fatalf("handle yolo command: %v", err)
	}

	waitFor(t, "yolo ack", func() bool {
		return telegram.messageCount() == 1
	})

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 2,
		Message: &telegramMessage{
			MessageID: 562,
			Chat:      privateChat(33),
			Text:      "Ship it",
		},
	}); err != nil {
		t.Fatalf("handle message: %v", err)
	}

	waitFor(t, "turn start", func() bool {
		return codex.startCount() == 1
	})

	ensureCalls := codex.ensureThreadCallsSnapshot()
	if len(ensureCalls) != 1 || ensureCalls[0].threadID != "new-thread-1" || !ensureCalls[0].options.Yolo {
		t.Fatalf("expected yolo ensureThread call for the fresh thread, got %#v", ensureCalls)
	}

	startCalls := codex.startCallsSnapshot()
	if len(startCalls) != 1 || startCalls[0].threadID != "new-thread-1" {
		t.Fatalf("unexpected start calls: %#v", startCalls)
	}

	codex.finishTurn("new-thread-1", turnResult{Text: "Done"})
	waitFor(t, "yolo turn completion", func() bool {
		return telegram.messageCount() == 2
	})
}

func TestLoadStateSupportsLegacyAndNewFormats(t *testing.T) {
	cfg := testConfig(t)

	legacyApp := newRelayAppWithServices(cfg, &fakeTelegramService{}, newFakeCodexService())
	legacyData := []byte("{\n  \"7\": \"thread-7\"\n}\n")
	if err := os.WriteFile(cfg.StatePath, legacyData, 0o644); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}
	if err := legacyApp.loadState(); err != nil {
		t.Fatalf("load legacy state: %v", err)
	}
	if got := legacyApp.threadIDForChat(7); got != "thread-7" {
		t.Fatalf("unexpected legacy thread id: %q", got)
	}
	if legacyApp.yoloForChat(7) {
		t.Fatal("legacy state should not enable yolo")
	}

	newApp := newRelayAppWithServices(cfg, &fakeTelegramService{}, newFakeCodexService())
	newData := []byte("{\n  \"threads\": {\n    \"9\": \"thread-9\"\n  },\n  \"verbose_by_chat\": {\n    \"9\": true\n  },\n  \"yolo_by_chat\": {\n    \"9\": true\n  },\n  \"service_tier_by_chat\": {\n    \"9\": \"default\"\n  },\n  \"continuity_by_chat\": {\n    \"9\": {\n      \"recent_messages\": [\n        {\n          \"role\": \"user\",\n          \"text\": \"Remember this\"\n        }\n      ],\n      \"pending_turn\": {\n        \"started_at\": \"2026-03-18T12:00:00Z\",\n        \"last_user_message\": \"Remember this\"\n      }\n    }\n  }\n}\n")
	if err := os.WriteFile(cfg.StatePath, newData, 0o644); err != nil {
		t.Fatalf("write new state: %v", err)
	}
	if err := newApp.loadState(); err != nil {
		t.Fatalf("load new state: %v", err)
	}
	if got := newApp.threadIDForChat(9); got != "thread-9" {
		t.Fatalf("unexpected new-format thread id: %q", got)
	}
	if !newApp.verboseForChat(9) {
		t.Fatal("new state should enable verbose")
	}
	if !newApp.yoloForChat(9) {
		t.Fatal("new state should enable yolo")
	}
	if newApp.fastModeForChat(9) {
		t.Fatal("new state should disable fast mode with default override")
	}
	continuity := newApp.continuityForChat(9)
	if len(continuity.RecentMessages) != 1 || continuity.RecentMessages[0].Text != "Remember this" {
		t.Fatalf("unexpected continuity state: %#v", continuity)
	}
	if continuity.PendingTurn == nil || continuity.PendingTurn.LastUserMessage != "Remember this" {
		t.Fatalf("unexpected pending turn continuity: %#v", continuity.PendingTurn)
	}

	modelApp := newRelayAppWithServices(cfg, &fakeTelegramService{}, newFakeCodexService())
	modelData := []byte("{\n  \"threads\": {\n    \"10\": \"thread-10\"\n  },\n  \"yolo_by_chat\": {\n    \"10\": true\n  },\n  \"model_by_chat\": {\n    \"10\": \"gpt-5\"\n  }\n}\n")
	if err := os.WriteFile(cfg.StatePath, modelData, 0o644); err != nil {
		t.Fatalf("write model state: %v", err)
	}
	if err := modelApp.loadState(); err != nil {
		t.Fatalf("load model state: %v", err)
	}
	if got := modelApp.threadIDForChat(10); got != "thread-10" {
		t.Fatalf("unexpected model-format thread id: %q", got)
	}
	if got := modelApp.modelOverrideForChat(10); got != "gpt-5" {
		t.Fatalf("unexpected model override: %q", got)
	}
}

func TestVerboseModePersistsAcrossReload(t *testing.T) {
	cfg := testConfig(t)
	app := newRelayAppWithServices(cfg, &fakeTelegramService{}, newFakeCodexService())

	enabled, message := app.toggleVerboseForChat(42, "/verbose on")
	if !enabled {
		t.Fatal("expected verbose mode to be enabled")
	}
	if message != "" {
		t.Fatalf("unexpected verbose message: %q", message)
	}

	reloaded := newRelayAppWithServices(cfg, &fakeTelegramService{}, newFakeCodexService())
	if err := reloaded.loadState(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	if !reloaded.verboseForChat(42) {
		t.Fatal("expected verbose mode to persist across reload")
	}
}

func TestCompletedTurnsPersistRecentContinuityAcrossReload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := testConfig(t)
	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(cfg, telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 701,
			Chat:      privateChat(7),
			Text:      "Remember this plan",
		},
	}); err != nil {
		t.Fatalf("handle message: %v", err)
	}

	waitFor(t, "turn start", func() bool {
		return codex.startCount() == 1
	})

	codex.finishTurn("thread-1", turnResult{Text: "Plan captured"})

	waitFor(t, "telegram reply", func() bool {
		return telegram.messageCount() == 1
	})

	reloaded := newRelayAppWithServices(cfg, &fakeTelegramService{}, newFakeCodexService())
	if err := reloaded.loadState(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	continuity := reloaded.continuityForChat(7)
	if continuity.PendingTurn != nil {
		t.Fatalf("expected pending turn to clear after completion, got %#v", continuity.PendingTurn)
	}
	if len(continuity.RecentMessages) != 2 {
		t.Fatalf("expected two recent messages, got %#v", continuity.RecentMessages)
	}
	if continuity.RecentMessages[0].Role != "user" || continuity.RecentMessages[0].Text != "Remember this plan" {
		t.Fatalf("unexpected first continuity message: %#v", continuity.RecentMessages[0])
	}
	if continuity.RecentMessages[1].Role != "assistant" || continuity.RecentMessages[1].Text != "Plan captured" {
		t.Fatalf("unexpected second continuity message: %#v", continuity.RecentMessages[1])
	}
}

func TestFailedResumeBootstrapsNewThreadFromContinuity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	codex.forceEnsureFallback("thread-7")
	app := newRelayAppWithServices(testConfig(t), telegram, codex)
	app.threadIDsByChat["7"] = "thread-7"
	app.continuityByChat[7] = chatContinuityState{
		RecentMessages: []relayMessageSnapshot{
			{Role: "user", Text: "Earlier request"},
			{Role: "assistant", Text: "Earlier answer"},
		},
		PendingTurn: &pendingTurnSnapshot{
			StartedAt:       "2026-03-18T12:00:00Z",
			LastUserMessage: "Earlier request",
		},
	}

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 2,
		Message: &telegramMessage{
			MessageID: 702,
			Chat:      privateChat(7),
			Text:      "Continue from there",
		},
	}); err != nil {
		t.Fatalf("handle message: %v", err)
	}

	waitFor(t, "fallback turn start", func() bool {
		return codex.startCount() == 1
	})

	startCalls := codex.startCallsSnapshot()
	if len(startCalls) != 1 {
		t.Fatalf("unexpected start calls: %#v", startCalls)
	}
	if startCalls[0].threadID != "new-thread-1" {
		t.Fatalf("expected fallback to new thread, got %#v", startCalls[0])
	}
	if !stringsContains(startCalls[0].text, "could not be resumed") {
		t.Fatalf("expected fallback bootstrap note, got %q", startCalls[0].text)
	}
	if !stringsContains(startCalls[0].text, "User: Earlier request") || !stringsContains(startCalls[0].text, "Assistant: Earlier answer") {
		t.Fatalf("expected recent continuity in fallback bootstrap, got %q", startCalls[0].text)
	}
	if !stringsContains(startCalls[0].text, "Latest user text:\nContinue from there") {
		t.Fatalf("expected latest user text in fallback bootstrap, got %q", startCalls[0].text)
	}
	if got := app.threadIDForChat(7); got != "new-thread-1" {
		t.Fatalf("expected saved thread to update after fallback, got %q", got)
	}
}

func TestNewCommandClearsContinuity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := testConfig(t)
	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(cfg, telegram, codex)
	app.continuityByChat[7] = chatContinuityState{
		RecentMessages: []relayMessageSnapshot{
			{Role: "user", Text: "Keep me?"},
		},
		PendingTurn: &pendingTurnSnapshot{
			StartedAt:       "2026-03-18T12:00:00Z",
			LastUserMessage: "Keep me?",
		},
	}

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 3,
		Message: &telegramMessage{
			MessageID: 703,
			Chat:      privateChat(7),
			Text:      "/new",
		},
	}); err != nil {
		t.Fatalf("handle command: %v", err)
	}

	waitFor(t, "continuity cleared", func() bool {
		continuity := app.continuityForChat(7)
		return len(continuity.RecentMessages) == 0 && continuity.PendingTurn == nil
	})

	continuity := app.continuityForChat(7)
	if len(continuity.RecentMessages) != 0 || continuity.PendingTurn != nil {
		t.Fatalf("expected /new to clear continuity, got %#v", continuity)
	}

	reloaded := newRelayAppWithServices(cfg, &fakeTelegramService{}, newFakeCodexService())
	if err := reloaded.loadState(); err != nil {
		t.Fatalf("load state: %v", err)
	}
	reloadedContinuity := reloaded.continuityForChat(7)
	if len(reloadedContinuity.RecentMessages) != 0 || reloadedContinuity.PendingTurn != nil {
		t.Fatalf("expected cleared continuity after reload, got %#v", reloadedContinuity)
	}
}

func TestActiveTurnSendsTypingAction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 601,
			Chat:      privateChat(41),
			Text:      "Generate something slow",
		},
	}); err != nil {
		t.Fatalf("handle message: %v", err)
	}

	waitFor(t, "typing action", func() bool {
		return telegram.actionCount() > 0
	})

	action := telegram.chatActions()[0]
	if action.chatID != 41 || action.action != "typing" {
		t.Fatalf("unexpected chat action: %#v", action)
	}

	codex.finishTurn("thread-1", turnResult{Text: "Done"})
	waitFor(t, "typing turn completion", func() bool {
		return telegram.messageCount() == 1
	})
}

func TestNonPrivateChatsAreIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 610,
			Chat:      telegramChat{ID: 99, Type: "group"},
			Text:      "/yolo on",
		},
	}); err != nil {
		t.Fatalf("handle group message: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	if telegram.messageCount() != 0 {
		t.Fatalf("expected no telegram replies for non-private chats, got %d", telegram.messageCount())
	}
	if codex.startCount() != 0 {
		t.Fatalf("expected no codex turn for non-private chats, got %d", codex.startCount())
	}
	if len(codex.newThreadCallsSnapshot()) != 0 {
		t.Fatalf("expected no new thread for non-private chats, got %#v", codex.newThreadCallsSnapshot())
	}
}

func TestDisallowedPrivateChatsAreIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	cfg := testConfig(t)
	cfg.TelegramAllowedChatIDs = map[int64]struct{}{7: {}}
	app := newRelayAppWithServices(cfg, telegram, codex)

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 611,
			Chat:      privateChat(99),
			Text:      "Hello",
		},
	}); err != nil {
		t.Fatalf("handle disallowed private message: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	if telegram.messageCount() != 0 {
		t.Fatalf("expected no telegram replies for disallowed private chats, got %d", telegram.messageCount())
	}
	if codex.startCount() != 0 {
		t.Fatalf("expected no codex turn for disallowed private chats, got %d", codex.startCount())
	}
}

func TestHandleUpdateDoesNotBlockWhenWorkerQueueIsFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	telegram := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), telegram, codex)

	worker := &chatWorker{
		app:    app,
		chatID: 7,
		events: make(chan chatEvent, 1),
	}
	worker.events <- chatEvent{messageID: 1, text: "queued"}
	app.workers[7] = worker

	done := make(chan error, 1)
	go func() {
		done <- app.handleUpdate(ctx, telegramUpdate{
			UpdateID: 1,
			Message: &telegramMessage{
				MessageID: 612,
				Chat:      privateChat(7),
				Text:      "overflow",
			},
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handle update returned error: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("handle update blocked on a full worker queue")
	}

	waitFor(t, "overflow notice", func() bool {
		return telegram.messageCount() == 1
	})
}

func TestPhotoMessagePassedToCodex(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tg := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), tg, codex)

	update := telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 501,
			Chat:      privateChat(7),
			Photo: []telegrampkg.PhotoSize{
				{FileID: "small-id", Width: 90, Height: 90},
				{FileID: "large-id", Width: 1280, Height: 720},
			},
			Caption: "What do you see?",
		},
	}
	if err := app.handleUpdate(ctx, update); err != nil {
		t.Fatalf("handle update: %v", err)
	}

	waitFor(t, "turn start", func() bool {
		return codex.startCount() == 1
	})

	calls := codex.startCallsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 start call, got %d", len(calls))
	}
	call := calls[0]
	if call.text != "What do you see?" {
		t.Fatalf("expected caption as text, got %q", call.text)
	}
	if len(call.imagePaths) != 1 {
		t.Fatalf("expected 1 image path, got %d", len(call.imagePaths))
	}
	// The path should be a non-empty string (a temp file path).
	if call.imagePaths[0] == "" {
		t.Fatal("expected non-empty image path")
	}

	codex.finishTurn(call.threadID, turnResult{Text: "I see a photo"})
	waitFor(t, "telegram reply", func() bool {
		return tg.messageCount() == 1
	})
}

func TestPhotoOnlyMessageWithoutCaptionPassedToCodex(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tg := &fakeTelegramService{}
	codex := newFakeCodexService()
	app := newRelayAppWithServices(testConfig(t), tg, codex)

	update := telegramUpdate{
		UpdateID: 1,
		Message: &telegramMessage{
			MessageID: 502,
			Chat:      privateChat(7),
			Photo: []telegrampkg.PhotoSize{
				{FileID: "photo-only-id", Width: 640, Height: 480},
			},
		},
	}
	if err := app.handleUpdate(ctx, update); err != nil {
		t.Fatalf("handle update: %v", err)
	}

	waitFor(t, "turn start", func() bool {
		return codex.startCount() == 1
	})

	calls := codex.startCallsSnapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 start call, got %d", len(calls))
	}
	call := calls[0]
	if call.text != "" {
		t.Fatalf("expected empty text for photo-only message, got %q", call.text)
	}
	if len(call.imagePaths) != 1 {
		t.Fatalf("expected 1 image path, got %d", len(call.imagePaths))
	}
	if call.imagePaths[0] == "" {
		t.Fatal("expected non-empty image path")
	}
	codex.finishTurn(call.threadID, turnResult{Text: "Image received"})
}

func TestSaveStateUsesPrivatePermissions(t *testing.T) {
	cfg := testConfig(t)
	cfg.StatePath = filepath.Join(t.TempDir(), "state", "relay.json")

	app := newRelayAppWithServices(cfg, &fakeTelegramService{}, newFakeCodexService())
	app.threadIDsByChat["7"] = "thread-7"

	if err := app.saveState(); err != nil {
		t.Fatalf("save state: %v", err)
	}

	stateInfo, err := os.Stat(cfg.StatePath)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if got := stateInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("expected state file mode 0600, got %o", got)
	}

	dirInfo, err := os.Stat(filepath.Dir(cfg.StatePath))
	if err != nil {
		t.Fatalf("stat state directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("expected state directory mode 0700, got %o", got)
	}
}

type fakeTelegramService struct {
	mu                  sync.Mutex
	sent                []sentTelegramMessage
	actions             []sentTelegramAction
	getUpdatesCalls     int
	getUpdatesResponses []fakeGetUpdatesResponse
}

type sentTelegramMessage struct {
	chatID int64
	text   string
}

type sentTelegramAction struct {
	chatID int64
	action string
}

type fakeGetUpdatesResponse struct {
	updates []telegramUpdate
	err     error
}

func (f *fakeTelegramService) DeleteWebhook(ctx context.Context, dropPending bool) error {
	return nil
}

func (f *fakeTelegramService) GetUpdates(ctx context.Context, offset *int64, timeoutSeconds int) ([]telegramUpdate, error) {
	f.mu.Lock()
	f.getUpdatesCalls++
	if len(f.getUpdatesResponses) > 0 {
		response := f.getUpdatesResponses[0]
		f.getUpdatesResponses = f.getUpdatesResponses[1:]
		f.mu.Unlock()
		return response.updates, response.err
	}
	f.mu.Unlock()

	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakeTelegramService) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	return []byte("fake-image-bytes-for-" + fileID), ".jpg", nil
}

func (f *fakeTelegramService) SendMessage(ctx context.Context, chatID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentTelegramMessage{
		chatID: chatID,
		text:   text,
	})
	return nil
}

func (f *fakeTelegramService) SendChatAction(ctx context.Context, chatID int64, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, sentTelegramAction{
		chatID: chatID,
		action: action,
	})
	return nil
}

func (f *fakeTelegramService) SetMyCommands(ctx context.Context, commands []telegrampkg.BotCommand) error {
	return nil
}

func (f *fakeTelegramService) messageCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeTelegramService) messages() []sentTelegramMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentTelegramMessage, len(f.sent))
	copy(out, f.sent)
	return out
}

func (f *fakeTelegramService) actionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.actions)
}

func (f *fakeTelegramService) chatActions() []sentTelegramAction {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentTelegramAction, len(f.actions))
	copy(out, f.actions)
	return out
}

func (f *fakeTelegramService) queueGetUpdatesResponse(updates []telegramUpdate, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getUpdatesResponses = append(f.getUpdatesResponses, fakeGetUpdatesResponse{
		updates: updates,
		err:     err,
	})
}

func (f *fakeTelegramService) getUpdatesCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getUpdatesCalls
}

type fakeCodexService struct {
	mu                sync.Mutex
	turns             map[string]chan turnResult
	events            map[string]chan turnStreamEvent
	newThreadCalls    []fakeThreadCall
	ensureThreadCalls []fakeThreadCall
	compactCalls      []string
	startCalls        []fakeStartCall
	steerCalls        []fakeSteerCall
	ensureFallbackFor map[string]bool
	nextThread        int
}

type fakeStartCall struct {
	threadID   string
	text       string
	imagePaths []string
}

type fakeThreadCall struct {
	threadID string
	options  codexThreadOptions
}

type fakeSteerCall struct {
	threadID   string
	turnID     string
	text       string
	imagePaths []string
}

func newFakeCodexService() *fakeCodexService {
	return &fakeCodexService{
		turns:             make(map[string]chan turnResult),
		events:            make(map[string]chan turnStreamEvent),
		ensureFallbackFor: make(map[string]bool),
	}
}

func (f *fakeCodexService) Close() error {
	return nil
}

func (f *fakeCodexService) NewThread(ctx context.Context, options codexThreadOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextThread++
	threadID := fmt.Sprintf("new-thread-%d", f.nextThread)
	f.newThreadCalls = append(f.newThreadCalls, fakeThreadCall{
		threadID: threadID,
		options:  options,
	})
	return threadID, nil
}

func (f *fakeCodexService) EnsureThread(ctx context.Context, threadID string, options codexThreadOptions) (string, error) {
	if threadID != "" {
		f.mu.Lock()
		f.ensureThreadCalls = append(f.ensureThreadCalls, fakeThreadCall{
			threadID: threadID,
			options:  options,
		})
		if f.ensureFallbackFor[threadID] {
			f.nextThread++
			threadID = fmt.Sprintf("new-thread-%d", f.nextThread)
			f.newThreadCalls = append(f.newThreadCalls, fakeThreadCall{
				threadID: threadID,
				options:  options,
			})
			f.mu.Unlock()
			return threadID, nil
		}
		f.mu.Unlock()
		return threadID, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextThread++
	threadID = fmt.Sprintf("thread-%d", f.nextThread)
	f.newThreadCalls = append(f.newThreadCalls, fakeThreadCall{
		threadID: threadID,
		options:  options,
	})
	return threadID, nil
}

func (f *fakeCodexService) CompactThread(ctx context.Context, threadID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.compactCalls = append(f.compactCalls, threadID)
	return nil
}

func (f *fakeCodexService) StartTurn(ctx context.Context, threadID string, userText string, imagePaths []string) (*codexTurnHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	resultCh := make(chan turnResult, 1)
	eventCh := make(chan turnStreamEvent, 8)
	f.turns[threadID] = resultCh
	f.events[threadID] = eventCh
	f.startCalls = append(f.startCalls, fakeStartCall{
		threadID:   threadID,
		text:       userText,
		imagePaths: imagePaths,
	})
	return &codexTurnHandle{
		ThreadID: threadID,
		TurnID:   fmt.Sprintf("turn-%d", len(f.startCalls)),
		EventCh:  eventCh,
		ResultCh: resultCh,
	}, nil
}

func (f *fakeCodexService) SteerTurn(ctx context.Context, threadID string, turnID string, userText string, imagePaths []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steerCalls = append(f.steerCalls, fakeSteerCall{
		threadID:   threadID,
		turnID:     turnID,
		text:       userText,
		imagePaths: imagePaths,
	})
	return nil
}

func (f *fakeCodexService) finishTurn(threadID string, result turnResult) {
	f.mu.Lock()
	resultCh := f.turns[threadID]
	eventCh := f.events[threadID]
	f.mu.Unlock()
	close(eventCh)
	resultCh <- result
	close(resultCh)
}

func (f *fakeCodexService) emitTurnEvent(threadID string, event turnStreamEvent) {
	f.mu.Lock()
	eventCh := f.events[threadID]
	f.mu.Unlock()
	eventCh <- event
}

func (f *fakeCodexService) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.startCalls)
}

func (f *fakeCodexService) steerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.steerCalls)
}

func (f *fakeCodexService) newThreadCallsSnapshot() []fakeThreadCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeThreadCall, len(f.newThreadCalls))
	copy(out, f.newThreadCalls)
	return out
}

func (f *fakeCodexService) ensureThreadCallsSnapshot() []fakeThreadCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeThreadCall, len(f.ensureThreadCalls))
	copy(out, f.ensureThreadCalls)
	return out
}

func (f *fakeCodexService) compactCallsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.compactCalls))
	copy(out, f.compactCalls)
	return out
}

func (f *fakeCodexService) startCallsSnapshot() []fakeStartCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeStartCall, len(f.startCalls))
	copy(out, f.startCalls)
	return out
}

func (f *fakeCodexService) steerCallsSnapshot() []fakeSteerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeSteerCall, len(f.steerCalls))
	copy(out, f.steerCalls)
	return out
}

func (f *fakeCodexService) forceEnsureFallback(threadID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureFallbackFor[threadID] = true
}

func testConfig(t *testing.T) configpkg.Config {
	t.Helper()
	return configpkg.Config{
		TelegramBotToken: "token",
		TelegramAllowedChatIDs: map[int64]struct{}{
			7:  {},
			11: {},
			12: {},
			21: {},
			22: {},
			31: {},
			32: {},
			33: {},
			34: {},
			35: {},
			36: {},
			37: {},
			41: {},
		},
		TelegramPollTimeoutSeconds: 30,
		TelegramMessageChunkSize:   3900,
		StatePath:                  filepath.Join(t.TempDir(), ".relay-state.json"),
		CodexCWD:                   t.TempDir(),
		CodexStartAppServer:        true,
		CodexAppServerCommand:      []string{"codex", "app-server"},
		CodexModel:                 "gpt-5.4",
		CodexPersonality:           "pragmatic",
		CodexSandbox:               "workspace-write",
		CodexApprovalPolicy:        "never",
		CodexServiceTier:           "fast",
	}
}

func waitFor(t *testing.T, label string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", label)
}
