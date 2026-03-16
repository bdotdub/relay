package main

import (
	"context"
	"sync"
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
