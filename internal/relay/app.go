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
	sleep    func(context.Context, time.Duration) error

	stateMu           sync.RWMutex
	threadIDsByChat   map[string]string
	verboseByChat     map[int64]bool
	yoloByChat        map[int64]bool
	serviceTierByChat map[int64]string
	modelByChat       map[int64]string
	continuityByChat  map[int64]chatContinuityState
	lastUsageByChat   map[int64]codex.TokenUsage

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
	messageID  int64
	rawText    string
	text       string
	imagePaths []string
	isCommand  bool
}

type activeChatTurn struct {
	threadID       string
	turnID         string
	replyMessageID int64
	eventCh        <-chan codex.TurnStreamEvent
	resultCh       <-chan codex.TurnResult
	stopTyping     func()
	tmpFiles       []string // temp image files to remove after the turn
	inputs         []turnReplayInput
	retryCount     int
}

type turnReplayInput struct {
	text       string
	imagePaths []string
}

type relayMessageSnapshot struct {
	Role      string `json:"role,omitempty"`
	Text      string `json:"text,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

type pendingTurnSnapshot struct {
	StartedAt       string `json:"started_at,omitempty"`
	LastUserMessage string `json:"last_user_message,omitempty"`
}

type chatContinuityState struct {
	RecentMessages []relayMessageSnapshot `json:"recent_messages,omitempty"`
	PendingTurn    *pendingTurnSnapshot   `json:"pending_turn,omitempty"`
}

type relayState struct {
	Threads           map[string]string              `json:"threads,omitempty"`
	VerboseByChat     map[string]bool                `json:"verbose_by_chat,omitempty"`
	YoloByChat        map[string]bool                `json:"yolo_by_chat,omitempty"`
	ServiceTierByChat map[string]string              `json:"service_tier_by_chat,omitempty"`
	ModelByChat       map[string]string              `json:"model_by_chat,omitempty"`
	ContinuityByChat  map[string]chatContinuityState `json:"continuity_by_chat,omitempty"`
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
		cfg:               cfg,
		telegram:          telegramSvc,
		codex:             codexSvc,
		reload:            reloadCurrentProcess,
		sleep:             sleepContext,
		threadIDsByChat:   map[string]string{},
		verboseByChat:     map[int64]bool{},
		yoloByChat:        map[int64]bool{},
		serviceTierByChat: map[int64]string{},
		modelByChat:       map[int64]string{},
		continuityByChat:  map[int64]chatContinuityState{},
		lastUsageByChat:   map[int64]codex.TokenUsage{},
		workers:           map[int64]*chatWorker{},
	}
}
