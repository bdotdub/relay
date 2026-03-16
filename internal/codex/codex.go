package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/bdotdub/relay/internal/config"
	"github.com/bdotdub/relay/internal/jsonrpc"
	"github.com/bdotdub/relay/internal/logx"
)

type Service interface {
	Close() error
	NewThread(ctx context.Context, options ThreadOptions) (string, error)
	EnsureThread(ctx context.Context, threadID string, options ThreadOptions) (string, error)
	StartTurn(ctx context.Context, threadID string, userText string) (*TurnHandle, error)
	SteerTurn(ctx context.Context, threadID string, turnID string, userText string) error
}

type ThreadOptions struct {
	Yolo  bool
	Model string
}

type Client struct {
	rpc           *jsonrpc.Client
	cfg           config.Config
	loadedThreads map[string]struct{}
	loadedMu      sync.Mutex
	activeTurns   map[string]*turnSubscription
	activeTurnsMu sync.Mutex
}

type TurnHandle struct {
	ThreadID string
	TurnID   string
	EventCh  <-chan TurnStreamEvent
	ResultCh <-chan TurnResult
}

type turnSubscription struct {
	threadID      string
	turnID        string
	notifications chan jsonrpc.Notification
	eventCh       chan TurnStreamEvent
	resultCh      chan TurnResult
	stopCh        chan error
}

type TurnStreamEvent struct {
	Text string
}

type TurnResult struct {
	Text             string
	ErrorMessage     string
	CommentaryText   string
	PlanText         string
	ReasoningSummary string
	Usage            *TokenUsage
	Err              error
}

type TokenUsage struct {
	Input  int64
	Output int64
	Total  int64
}

type initializeParams struct {
	ClientInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"clientInfo"`
}

type threadIDResponse struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
}

type turnIDResponse struct {
	Turn struct {
		ID string `json:"id"`
	} `json:"turn"`
}

type turnInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type turnStartParams struct {
	ThreadID string      `json:"threadId"`
	Input    []turnInput `json:"input"`
}

type turnSteerParams struct {
	ThreadID       string      `json:"threadId"`
	ExpectedTurnID string      `json:"expectedTurnId"`
	Input          []turnInput `json:"input"`
}

type threadNotificationParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId,omitempty"`
}

type deltaNotificationParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId,omitempty"`
	Delta    string `json:"delta"`
}

type completedItemParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId,omitempty"`
	Item     struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Phase string `json:"phase"`
	} `json:"item"`
}

type errorNotificationParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId,omitempty"`
	Error    struct {
		Message string `json:"message"`
	} `json:"error"`
}

type usageCarrier struct {
	Usage *protocolUsage `json:"usage,omitempty"`
	Turn  *struct {
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
		Usage *protocolUsage `json:"usage,omitempty"`
	} `json:"turn,omitempty"`
}

type turnCompletedParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId,omitempty"`
	usageCarrier
}

type protocolUsage struct {
	InputTokens       *int64 `json:"inputTokens,omitempty"`
	InputTokensSnake  *int64 `json:"input_tokens,omitempty"`
	PromptTokens      *int64 `json:"promptTokens,omitempty"`
	PromptTokensSnake *int64 `json:"prompt_tokens,omitempty"`

	OutputTokens          *int64 `json:"outputTokens,omitempty"`
	OutputTokensSnake     *int64 `json:"output_tokens,omitempty"`
	CompletionTokens      *int64 `json:"completionTokens,omitempty"`
	CompletionTokensSnake *int64 `json:"completion_tokens,omitempty"`

	TotalTokens      *int64 `json:"totalTokens,omitempty"`
	TotalTokensSnake *int64 `json:"total_tokens,omitempty"`
}

const relayDeveloperInstructions = "This Codex session is relayed through Telegram, and the user interacts with it there. Telegram messages are rendered with MarkdownV2. When you include a link, prefer the Markdown link form \"[label](url)\" so it renders correctly in Telegram. Do not include local filesystem paths unless they are truly necessary, because the user is interacting through Telegram rather than a shared local workspace."

func NewClient(ctx context.Context, cfg config.Config) (*Client, error) {
	var rpc *jsonrpc.Client
	var err error
	if cfg.CodexStartAppServer {
		rpc, err = jsonrpc.NewStdioClient(ctx, cfg.CodexAppServerCommand)
	} else {
		rpc, err = jsonrpc.NewWebSocketClient(ctx, cfg.CodexAppServerWSURL)
	}
	if err != nil {
		return nil, err
	}

	client := &Client{
		rpc:           rpc,
		cfg:           cfg,
		loadedThreads: make(map[string]struct{}),
		activeTurns:   make(map[string]*turnSubscription),
	}
	if err := client.initialize(ctx); err != nil {
		_ = rpc.Close()
		return nil, err
	}
	logx.Debug("codex client ready")
	go client.dispatchNotifications(ctx)
	return client, nil
}

func (c *Client) Close() error {
	if c.rpc == nil {
		return nil
	}
	return c.rpc.Close()
}

func (c *Client) initialize(ctx context.Context) error {
	params := initializeParams{}
	params.ClientInfo.Name = "telegram-codex-relay"
	params.ClientInfo.Version = "0.1.0"
	if err := c.rpc.Request(ctx, "initialize", params, nil); err != nil {
		return err
	}
	return c.rpc.Notify("initialized", nil)
}

func (c *Client) NewThread(ctx context.Context, options ThreadOptions) (string, error) {
	var response threadIDResponse
	if err := c.rpc.Request(ctx, "thread/start", c.newThreadParams(options), &response); err != nil {
		return "", err
	}
	threadID := response.Thread.ID
	if threadID == "" {
		return "", errors.New("missing thread.id")
	}
	c.loadedMu.Lock()
	c.loadedThreads[threadID] = struct{}{}
	c.loadedMu.Unlock()
	logx.Debug("codex thread started", "thread_id", threadID)
	return threadID, nil
}

func (c *Client) EnsureThread(ctx context.Context, threadID string, options ThreadOptions) (string, error) {
	if threadID == "" {
		return c.NewThread(ctx, options)
	}
	c.loadedMu.Lock()
	_, ok := c.loadedThreads[threadID]
	c.loadedMu.Unlock()
	if ok {
		return threadID, nil
	}

	params := c.resumeThreadParams(options)
	params["threadId"] = threadID
	if err := c.rpc.Request(ctx, "thread/resume", params, nil); err == nil {
		c.loadedMu.Lock()
		c.loadedThreads[threadID] = struct{}{}
		c.loadedMu.Unlock()
		logx.Debug("codex thread resumed", "thread_id", threadID)
		return threadID, nil
	}
	logx.Debug("codex thread resume failed; starting new thread", "thread_id", threadID)
	return c.NewThread(ctx, options)
}

func (c *Client) StartTurn(ctx context.Context, threadID string, userText string) (*TurnHandle, error) {
	subscription := &turnSubscription{
		threadID:      threadID,
		notifications: make(chan jsonrpc.Notification, 128),
		eventCh:       make(chan TurnStreamEvent, 32),
		resultCh:      make(chan TurnResult, 1),
		stopCh:        make(chan error, 1),
	}

	c.activeTurnsMu.Lock()
	if _, exists := c.activeTurns[threadID]; exists {
		c.activeTurnsMu.Unlock()
		return nil, fmt.Errorf("thread %s already has an active turn", threadID)
	}
	c.activeTurns[threadID] = subscription
	c.activeTurnsMu.Unlock()

	params := turnStartParams{
		ThreadID: threadID,
		Input:    []turnInput{{Type: "text", Text: userText}},
	}
	var response turnIDResponse
	if err := c.rpc.Request(ctx, "turn/start", params, &response); err != nil {
		c.removeActiveTurn(threadID)
		return nil, err
	}

	turnID := response.Turn.ID
	if turnID == "" {
		c.removeActiveTurn(threadID)
		return nil, errors.New("missing turn.id")
	}
	subscription.turnID = turnID
	logx.Debug("codex turn started", "thread_id", threadID, "turn_id", turnID, "text", logx.SummarizeText(userText))
	go c.collectTurnResult(subscription)

	return &TurnHandle{
		ThreadID: threadID,
		TurnID:   turnID,
		EventCh:  subscription.eventCh,
		ResultCh: subscription.resultCh,
	}, nil
}

func (c *Client) SteerTurn(ctx context.Context, threadID string, turnID string, userText string) error {
	logx.Debug("codex turn steer", "thread_id", threadID, "turn_id", turnID, "text", logx.SummarizeText(userText))
	return c.rpc.Request(ctx, "turn/steer", turnSteerParams{
		ThreadID:       threadID,
		ExpectedTurnID: turnID,
		Input:          []turnInput{{Type: "text", Text: userText}},
	}, nil)
}

func (c *Client) dispatchNotifications(ctx context.Context) {
	for {
		notification, err := c.rpc.NextNotification(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				c.failActiveTurns(err)
			}
			return
		}

		var params threadNotificationParams
		if err := json.Unmarshal(notification.Params, &params); err != nil {
			continue
		}
		if params.ThreadID == "" {
			continue
		}

		c.activeTurnsMu.Lock()
		subscription := c.activeTurns[params.ThreadID]
		c.activeTurnsMu.Unlock()
		if subscription == nil {
			continue
		}
		logx.Debug("codex notification routed", "method", notification.Method, "thread_id", params.ThreadID, "turn_id", params.TurnID)

		select {
		case subscription.notifications <- notification:
		default:
			subscription.notifications <- notification
		}
	}
}

func (c *Client) collectTurnResult(subscription *turnSubscription) {
	defer c.removeActiveTurn(subscription.threadID)
	defer close(subscription.eventCh)

	var deltas string
	var completedMessages []agentMessage
	var errorMessage string
	var planDeltas []string
	var reasoningSummaryDeltas []string
	var usage *TokenUsage

	for {
		select {
		case err := <-subscription.stopCh:
			subscription.resultCh <- TurnResult{Err: err}
			close(subscription.resultCh)
			return
		case notification := <-subscription.notifications:
			switch notification.Method {
			case "item/agentMessage/delta":
				var params deltaNotificationParams
				if !decodeNotificationParams(notification, &params) || !matchesTurn(params.TurnID, subscription.turnID) {
					continue
				}
				deltas += params.Delta
			case "item/plan/delta":
				var params deltaNotificationParams
				if !decodeNotificationParams(notification, &params) || !matchesTurn(params.TurnID, subscription.turnID) {
					continue
				}
				if strings.TrimSpace(params.Delta) != "" {
					planDeltas = append(planDeltas, params.Delta)
				}
			case "item/reasoning/summaryTextDelta":
				var params deltaNotificationParams
				if !decodeNotificationParams(notification, &params) || !matchesTurn(params.TurnID, subscription.turnID) {
					continue
				}
				if strings.TrimSpace(params.Delta) != "" {
					reasoningSummaryDeltas = append(reasoningSummaryDeltas, params.Delta)
				}
			case "item/completed":
				var params completedItemParams
				if !decodeNotificationParams(notification, &params) || !matchesTurn(params.TurnID, subscription.turnID) {
					continue
				}
				if params.Item.Type != "agentMessage" {
					continue
				}
				completedMessages = append(completedMessages, agentMessage{
					phase: params.Item.Phase,
					text:  params.Item.Text,
				})
				if eventText := formatIntermediateTurnEvent(params.Item.Phase, params.Item.Text); eventText != "" {
					subscription.eventCh <- TurnStreamEvent{Text: eventText}
				}
			case "error":
				var params errorNotificationParams
				if !decodeNotificationParams(notification, &params) || !matchesTurn(params.TurnID, subscription.turnID) {
					continue
				}
				errorMessage = params.Error.Message
			case "turn/completed":
				var params turnCompletedParams
				if !decodeNotificationParams(notification, &params) || !matchesTurn(params.TurnID, subscription.turnID) {
					continue
				}
				if params.Turn != nil && errorMessage == "" && params.Turn.Status == "failed" && params.Turn.Error != nil {
					errorMessage = params.Turn.Error.Message
				}
				usage = extractTokenUsage(params.usageCarrier)
				result := TurnResult{
					Text:             finalTurnText(completedMessages, deltas),
					ErrorMessage:     errorMessage,
					CommentaryText:   commentaryText(completedMessages),
					PlanText:         strings.TrimSpace(strings.Join(planDeltas, "")),
					ReasoningSummary: strings.TrimSpace(strings.Join(reasoningSummaryDeltas, "")),
					Usage:            usage,
				}
				logx.Debug("codex turn completed",
					"thread_id", subscription.threadID,
					"turn_id", subscription.turnID,
					"usage", summarizeTokenUsage(result.Usage),
					"final_text", logx.SummarizeText(result.Text),
					"commentary", logx.SummarizeText(result.CommentaryText),
					"plan", logx.SummarizeText(result.PlanText),
					"reasoning_summary", logx.SummarizeText(result.ReasoningSummary),
				)
				subscription.resultCh <- result
				close(subscription.resultCh)
				return
			}
		}
	}
}

func (c *Client) removeActiveTurn(threadID string) {
	c.activeTurnsMu.Lock()
	delete(c.activeTurns, threadID)
	c.activeTurnsMu.Unlock()
}

func (c *Client) failActiveTurns(err error) {
	c.activeTurnsMu.Lock()
	defer c.activeTurnsMu.Unlock()
	for threadID, subscription := range c.activeTurns {
		delete(c.activeTurns, threadID)
		select {
		case subscription.stopCh <- err:
		default:
		}
	}
}

type agentMessage struct {
	phase string
	text  string
}

func finalTurnText(messages []agentMessage, deltas string) string {
	var finals []string
	var all []string
	for _, message := range messages {
		text := strings.TrimSpace(message.text)
		if text == "" {
			continue
		}
		all = append(all, text)
		if message.phase == "final_answer" {
			finals = append(finals, text)
		}
	}
	if len(finals) > 0 {
		return strings.Join(finals, "\n\n")
	}
	if len(all) > 0 {
		return strings.Join(all, "\n\n")
	}
	return strings.TrimSpace(deltas)
}

func commentaryText(messages []agentMessage) string {
	var commentary []string
	for _, message := range messages {
		if message.phase != "commentary" {
			continue
		}
		text := strings.TrimSpace(message.text)
		if text == "" {
			continue
		}
		commentary = append(commentary, text)
	}
	if len(commentary) == 0 {
		return ""
	}
	return strings.Join(commentary, "\n\n")
}

func formatIntermediateTurnEvent(phase string, text string) string {
	text = strings.TrimSpace(text)
	if text == "" || phase == "final_answer" {
		return ""
	}
	return text
}

func summarizeTokenUsage(usage *TokenUsage) string {
	if usage == nil {
		return "n/a"
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

func extractTokenUsage(payload usageCarrier) *TokenUsage {
	var usage TokenUsage
	var foundInput bool
	var foundOutput bool
	var foundTotal bool

	candidates := []*protocolUsage{payload.Usage}
	if payload.Turn != nil {
		candidates = append(candidates, payload.Turn.Usage)
	}

	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		if !foundInput {
			if value, ok := candidate.inputTokens(); ok {
				usage.Input = value
				foundInput = true
			}
		}
		if !foundOutput {
			if value, ok := candidate.outputTokens(); ok {
				usage.Output = value
				foundOutput = true
			}
		}
		if !foundTotal {
			if value, ok := candidate.totalTokens(); ok {
				usage.Total = value
				foundTotal = true
			}
		}
	}

	if !foundTotal && foundInput && foundOutput {
		usage.Total = usage.Input + usage.Output
		foundTotal = true
	}
	if !foundInput && !foundOutput && !foundTotal {
		return nil
	}
	return &usage
}

func (c *Client) baseThreadParams(options ThreadOptions) map[string]any {
	params := map[string]any{
		"cwd": c.cfg.CodexCWD,
	}
	if options.Yolo {
		params["approvalPolicy"] = "never"
		params["sandbox"] = "danger-full-access"
	} else {
		insertOptionalString(params, "approvalPolicy", c.cfg.CodexApprovalPolicy)
		insertOptionalString(params, "sandbox", c.cfg.CodexSandbox)
	}
	insertOptionalString(params, "model", c.modelForOptions(options))
	insertOptionalString(params, "personality", c.cfg.CodexPersonality)
	insertOptionalString(params, "serviceTier", c.cfg.CodexServiceTier)
	insertOptionalString(params, "baseInstructions", c.cfg.CodexBaseInstructions)
	insertOptionalString(params, "developerInstructions", c.developerInstructions())
	if merged := c.mergedThreadConfig(options); len(merged) > 0 {
		params["config"] = merged
	}
	return params
}

func (c *Client) developerInstructions() string {
	parts := make([]string, 0, 2)
	if value := strings.TrimSpace(c.cfg.CodexDeveloperInstructions); value != "" {
		parts = append(parts, value)
	}
	parts = append(parts, relayDeveloperInstructions)
	return strings.Join(parts, "\n\n")
}

func (c *Client) modelForOptions(options ThreadOptions) string {
	if strings.TrimSpace(options.Model) != "" {
		return options.Model
	}
	return c.cfg.CodexModel
}

func (c *Client) newThreadParams(options ThreadOptions) map[string]any {
	params := c.baseThreadParams(options)
	params["ephemeral"] = c.cfg.CodexEphemeralThreads
	return params
}

func (c *Client) resumeThreadParams(options ThreadOptions) map[string]any {
	return c.baseThreadParams(options)
}

// mergedThreadConfig returns the thread config map, merging in a permission profile when
// codexNetworkEnabled, codexFsReadPaths, or codexFsWritePaths are set. Uses snake_case keys
// to match the Codex protocol (file_system, network, permission_profile).
func (c *Client) mergedThreadConfig(options ThreadOptions) map[string]any {
	merged := make(map[string]any)
	if c.cfg.CodexConfig != nil {
		for k, v := range c.cfg.CodexConfig {
			if options.Yolo && k == "permission_profile" {
				continue
			}
			merged[k] = v
		}
	}
	profile := c.buildPermissionProfile(options)
	if profile != nil {
		merged["permission_profile"] = profile
	}
	return merged
}

func (c *Client) buildPermissionProfile(options ThreadOptions) map[string]any {
	if options.Yolo {
		return nil
	}
	var network map[string]any
	switch strings.TrimSpace(strings.ToLower(c.cfg.CodexNetworkEnabled)) {
	case "true", "1", "yes":
		network = map[string]any{"enabled": true}
	case "false", "0", "no":
		network = map[string]any{"enabled": false}
	}
	hasFS := len(c.cfg.CodexFsReadPaths) > 0 || len(c.cfg.CodexFsWritePaths) > 0
	var fileSystem map[string]any
	if hasFS {
		fileSystem = make(map[string]any)
		if len(c.cfg.CodexFsReadPaths) > 0 {
			fileSystem["read"] = c.cfg.CodexFsReadPaths
		}
		if len(c.cfg.CodexFsWritePaths) > 0 {
			fileSystem["write"] = c.cfg.CodexFsWritePaths
		}
	}
	if network == nil && !hasFS {
		return nil
	}
	profile := make(map[string]any)
	if network != nil {
		profile["network"] = network
	}
	if fileSystem != nil {
		profile["file_system"] = fileSystem
	}
	return profile
}

func insertOptionalString(params map[string]any, key string, value string) {
	if strings.TrimSpace(value) != "" {
		params[key] = value
	}
}

func decodeNotificationParams[T any](notification jsonrpc.Notification, out *T) bool {
	if err := json.Unmarshal(notification.Params, out); err != nil {
		return false
	}
	return true
}

func matchesTurn(notificationTurnID string, activeTurnID string) bool {
	return notificationTurnID == "" || activeTurnID == "" || notificationTurnID == activeTurnID
}

func (u *protocolUsage) inputTokens() (int64, bool) {
	return firstSet(u.InputTokens, u.InputTokensSnake, u.PromptTokens, u.PromptTokensSnake)
}

func (u *protocolUsage) outputTokens() (int64, bool) {
	return firstSet(u.OutputTokens, u.OutputTokensSnake, u.CompletionTokens, u.CompletionTokensSnake)
}

func (u *protocolUsage) totalTokens() (int64, bool) {
	return firstSet(u.TotalTokens, u.TotalTokensSnake)
}

func firstSet(values ...*int64) (int64, bool) {
	for _, value := range values {
		if value != nil {
			return *value, true
		}
	}
	return 0, false
}
