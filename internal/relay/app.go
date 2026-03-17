package relay

import (
	"context"
	"sync"
	"time"

	"github.com/bdotdub/relay/internal/codex"
	"github.com/bdotdub/relay/internal/config"
	"github.com/bdotdub/relay/internal/telegram"
)

type relayApp struct {
	cfg      config.Config
	telegram telegram.Service
	codex    codex.Service
	reload   func() error

	stateMu         sync.RWMutex
	threadIDsByChat map[string]string
	verboseByChat   map[int64]bool
	yoloByChat      map[int64]bool
	modelByChat     map[int64]string
	lastUsageByChat map[int64]codex.TokenUsage

	workersMu sync.Mutex
	workers   map[int64]*chatWorker
}

type chatWorker struct {
	app    *relayApp
	chatID int64
	events chan chatEvent

	overflowMu           sync.Mutex
	nextOverflowNoticeAt time.Time
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
	eventCh        <-chan codex.TurnStreamEvent
	resultCh       <-chan codex.TurnResult
	stopTyping     func()
}

type relayState struct {
	Threads       map[string]string `json:"threads,omitempty"`
	VerboseByChat map[string]bool   `json:"verbose_by_chat,omitempty"`
	YoloByChat    map[string]bool   `json:"yolo_by_chat,omitempty"`
	ModelByChat   map[string]string `json:"model_by_chat,omitempty"`
}

func Run(ctx context.Context, cfg config.Config) error {
	app, err := newRelayApp(ctx, cfg)
	if err != nil {
		return err
	}
	return app.run(ctx)
}

func newRelayApp(ctx context.Context, cfg config.Config) (*relayApp, error) {
	telegramClient := telegram.NewClient(cfg.TelegramBotToken)
	codexClient, err := codex.NewClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return newRelayAppWithServices(cfg, telegramClient, codexClient), nil
}

func newRelayAppWithServices(cfg config.Config, telegramSvc telegram.Service, codexSvc codex.Service) *relayApp {
	return &relayApp{
		cfg:             cfg,
		telegram:        telegramSvc,
		codex:           codexSvc,
		reload:          reloadCurrentProcess,
		threadIDsByChat: map[string]string{},
		verboseByChat:   map[int64]bool{},
		yoloByChat:      map[int64]bool{},
		modelByChat:     map[int64]string{},
		lastUsageByChat: map[int64]codex.TokenUsage{},
		workers:         map[int64]*chatWorker{},
	}
}
