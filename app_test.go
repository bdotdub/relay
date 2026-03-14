package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

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
			Chat:      telegramChat{ID: 7},
			Text:      "Write a summary",
		},
	}
	secondMessage := telegramUpdate{
		UpdateID: 2,
		Message: &telegramMessage{
			MessageID: 102,
			Chat:      telegramChat{ID: 7},
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

	codex.finishTurn("thread-1", turnResult{text: "Final answer"})

	waitFor(t, "telegram reply", func() bool {
		return telegram.messageCount() == 1
	})

	message := telegram.messages()[0]
	if message.text != "Final answer" {
		t.Fatalf("unexpected reply text: %q", message.text)
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
				Chat:      telegramChat{ID: 11},
				Text:      "Chat one",
			},
		},
		{
			UpdateID: 2,
			Message: &telegramMessage{
				MessageID: 301,
				Chat:      telegramChat{ID: 12},
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
			Chat:      telegramChat{ID: 21},
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
			Chat:      telegramChat{ID: 21},
			Text:      "Do the thing",
		},
	}); err != nil {
		t.Fatalf("handle message: %v", err)
	}

	waitFor(t, "turn start", func() bool {
		return codex.startCount() == 1
	})

	codex.emitTurnEvent("thread-1", turnStreamEvent{text: "Working on it"})

	waitFor(t, "intermediate reply", func() bool {
		return telegram.messageCount() == 2
	})

	codex.finishTurn("thread-1", turnResult{
		text:             "Final answer",
		commentaryText:   "Working on it",
		planText:         "1. Inspect\n2. Change",
		reasoningSummary: "Used the existing structure and simplified the flow.",
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
			Chat:      telegramChat{ID: 22},
			Text:      "Do the thing",
		},
	}); err != nil {
		t.Fatalf("handle message: %v", err)
	}

	waitFor(t, "turn start", func() bool {
		return codex.startCount() == 1
	})

	codex.emitTurnEvent("thread-1", turnStreamEvent{text: "Working on it"})
	time.Sleep(50 * time.Millisecond)

	if telegram.messageCount() != 0 {
		t.Fatalf("expected no intermediate reply while verbose is off, got %d", telegram.messageCount())
	}

	codex.finishTurn("thread-1", turnResult{text: "Final answer"})

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
			Chat:      telegramChat{ID: 31},
			Text:      "Count tokens",
		},
	}); err != nil {
		t.Fatalf("handle message: %v", err)
	}

	waitFor(t, "turn start", func() bool {
		return codex.startCount() == 1
	})

	codex.finishTurn("thread-1", turnResult{
		text:  "Done",
		usage: &tokenUsage{input: 120, output: 34, total: 154},
	})

	waitFor(t, "turn reply", func() bool {
		return telegram.messageCount() == 1
	})

	if err := app.handleUpdate(ctx, telegramUpdate{
		UpdateID: 2,
		Message: &telegramMessage{
			MessageID: 502,
			Chat:      telegramChat{ID: 31},
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
			Chat:      telegramChat{ID: 41},
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

	codex.finishTurn("thread-1", turnResult{text: "Done"})
}

type fakeTelegramService struct {
	mu      sync.Mutex
	sent    []sentTelegramMessage
	actions []sentTelegramAction
}

type sentTelegramMessage struct {
	chatID int64
	text   string
}

type sentTelegramAction struct {
	chatID int64
	action string
}

func (f *fakeTelegramService) deleteWebhook(ctx context.Context, dropPending bool) error {
	return nil
}

func (f *fakeTelegramService) getUpdates(ctx context.Context, offset *int64, timeoutSeconds int) ([]telegramUpdate, error) {
	return nil, nil
}

func (f *fakeTelegramService) sendMessage(ctx context.Context, chatID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentTelegramMessage{
		chatID: chatID,
		text:   text,
	})
	return nil
}

func (f *fakeTelegramService) sendChatAction(ctx context.Context, chatID int64, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, sentTelegramAction{
		chatID: chatID,
		action: action,
	})
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

type fakeCodexService struct {
	mu         sync.Mutex
	turns      map[string]chan turnResult
	events     map[string]chan turnStreamEvent
	startCalls []fakeStartCall
	steerCalls []fakeSteerCall
	nextThread int
}

type fakeStartCall struct {
	threadID string
	text     string
}

type fakeSteerCall struct {
	threadID string
	turnID   string
	text     string
}

func newFakeCodexService() *fakeCodexService {
	return &fakeCodexService{
		turns:  make(map[string]chan turnResult),
		events: make(map[string]chan turnStreamEvent),
	}
}

func (f *fakeCodexService) close() error {
	return nil
}

func (f *fakeCodexService) newThread(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextThread++
	return fmt.Sprintf("new-thread-%d", f.nextThread), nil
}

func (f *fakeCodexService) ensureThread(ctx context.Context, threadID string) (string, error) {
	if threadID != "" {
		return threadID, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextThread++
	return fmt.Sprintf("thread-%d", f.nextThread), nil
}

func (f *fakeCodexService) startTurn(ctx context.Context, threadID string, userText string) (*codexTurnHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	resultCh := make(chan turnResult, 1)
	eventCh := make(chan turnStreamEvent, 8)
	f.turns[threadID] = resultCh
	f.events[threadID] = eventCh
	f.startCalls = append(f.startCalls, fakeStartCall{
		threadID: threadID,
		text:     userText,
	})
	return &codexTurnHandle{
		ThreadID: threadID,
		TurnID:   fmt.Sprintf("turn-%d", len(f.startCalls)),
		EventCh:  eventCh,
		ResultCh: resultCh,
	}, nil
}

func (f *fakeCodexService) steerTurn(ctx context.Context, threadID string, turnID string, userText string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steerCalls = append(f.steerCalls, fakeSteerCall{
		threadID: threadID,
		turnID:   turnID,
		text:     userText,
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

func testConfig(t *testing.T) config {
	t.Helper()
	return config{
		telegramBotToken:           "token",
		telegramPollTimeoutSeconds: 30,
		telegramMessageChunkSize:   3900,
		statePath:                  filepath.Join(t.TempDir(), ".relay-state.json"),
		codexCWD:                   t.TempDir(),
		codexStartAppServer:        true,
		codexAppServerCommand:      []string{"codex", "app-server"},
		codexPersonality:           "pragmatic",
		codexSandbox:               "workspace-write",
		codexApprovalPolicy:        "never",
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
