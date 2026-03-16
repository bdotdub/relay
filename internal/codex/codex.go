package codex

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	notifications chan map[string]any
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

const relayDeveloperInstructions = "This Codex session is relayed through Telegram, and the user interacts with it there. Telegram messages are rendered with MarkdownV2. When you include a link, prefer the Markdown link form \"[label](url)\" so it renders correctly in Telegram. Do not include local filesystem paths unless they are truly necessary, because the user is interacting through Telegram rather than a shared local workspace."

func NewClient(ctx context.Context, cfg config.Config) (*Client, error) {
	var rpc *jsonrpc.Client
	var err error
	if cfg.CodexStartAppServer {
		rpc, err = jsonrpc.NewStdioClient(ctx, cfg.CodexAppServerCommand)
	} else {
		rpc, err = jsonrpc.NewWebSocketClient(cfg.CodexAppServerWSURL)
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
	logx.Debugf("codex client ready")
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
	_, err := c.rpc.Request(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "telegram-codex-relay",
			"version": "0.1.0",
		},
	})
	if err != nil {
		return err
	}
	return c.rpc.Notify("initialized", nil)
}

func (c *Client) NewThread(ctx context.Context, options ThreadOptions) (string, error) {
	response, err := c.rpc.Request(ctx, "thread/start", c.newThreadParams(options))
	if err != nil {
		return "", err
	}
	threadID, err := extractNestedString(response, "thread", "id")
	if err != nil {
		return "", err
	}
	c.loadedMu.Lock()
	c.loadedThreads[threadID] = struct{}{}
	c.loadedMu.Unlock()
	logx.Debugf("codex thread started %s", logx.KVSummary("thread_id", threadID))
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
	if _, err := c.rpc.Request(ctx, "thread/resume", params); err == nil {
		c.loadedMu.Lock()
		c.loadedThreads[threadID] = struct{}{}
		c.loadedMu.Unlock()
		logx.Debugf("codex thread resumed %s", logx.KVSummary("thread_id", threadID))
		return threadID, nil
	}
	logx.Debugf("codex thread resume failed; starting new thread %s", logx.KVSummary("thread_id", threadID))
	return c.NewThread(ctx, options)
}

func (c *Client) StartTurn(ctx context.Context, threadID string, userText string) (*TurnHandle, error) {
	subscription := &turnSubscription{
		threadID:      threadID,
		notifications: make(chan map[string]any, 128),
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

	response, err := c.rpc.Request(ctx, "turn/start", map[string]any{
		"threadId": threadID,
		"input": []map[string]any{
			{
				"type": "text",
				"text": userText,
			},
		},
	})
	if err != nil {
		c.removeActiveTurn(threadID)
		return nil, err
	}

	turnID, err := extractNestedString(response, "turn", "id")
	if err != nil {
		c.removeActiveTurn(threadID)
		return nil, err
	}
	subscription.turnID = turnID
	logx.Debugf("codex turn started %s", logx.KVSummary("thread_id", threadID, "turn_id", turnID, "text", logx.SummarizeText(userText)))
	go c.collectTurnResult(subscription)

	return &TurnHandle{
		ThreadID: threadID,
		TurnID:   turnID,
		EventCh:  subscription.eventCh,
		ResultCh: subscription.resultCh,
	}, nil
}

func (c *Client) SteerTurn(ctx context.Context, threadID string, turnID string, userText string) error {
	logx.Debugf("codex turn steer %s", logx.KVSummary("thread_id", threadID, "turn_id", turnID, "text", logx.SummarizeText(userText)))
	_, err := c.rpc.Request(ctx, "turn/steer", map[string]any{
		"threadId":       threadID,
		"expectedTurnId": turnID,
		"input": []map[string]any{
			{
				"type": "text",
				"text": userText,
			},
		},
	})
	return err
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

		params, _ := notification["params"].(map[string]any)
		if params == nil {
			continue
		}
		threadID, _ := params["threadId"].(string)
		if threadID == "" {
			continue
		}

		c.activeTurnsMu.Lock()
		subscription := c.activeTurns[threadID]
		c.activeTurnsMu.Unlock()
		if subscription == nil {
			continue
		}
		turnID, _ := params["turnId"].(string)
		method, _ := notification["method"].(string)
		logx.Debugf("codex notification routed %s", logx.KVSummary("method", method, "thread_id", threadID, "turn_id", turnID))

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
			params, _ := notification["params"].(map[string]any)
			if params == nil {
				continue
			}
			if notificationTurnID, ok := params["turnId"].(string); ok && subscription.turnID != "" && notificationTurnID != subscription.turnID {
				continue
			}

			method, _ := notification["method"].(string)
			switch method {
			case "item/agentMessage/delta":
				delta, _ := params["delta"].(string)
				deltas += delta
			case "item/plan/delta":
				delta, _ := params["delta"].(string)
				if strings.TrimSpace(delta) != "" {
					planDeltas = append(planDeltas, delta)
				}
			case "item/reasoning/summaryTextDelta":
				delta, _ := params["delta"].(string)
				if strings.TrimSpace(delta) != "" {
					reasoningSummaryDeltas = append(reasoningSummaryDeltas, delta)
				}
			case "item/completed":
				item, _ := params["item"].(map[string]any)
				if item == nil {
					continue
				}
				if itemType, _ := item["type"].(string); itemType != "agentMessage" {
					continue
				}
				text, _ := item["text"].(string)
				phase, _ := item["phase"].(string)
				completedMessages = append(completedMessages, agentMessage{
					phase: phase,
					text:  text,
				})
				if eventText := formatIntermediateTurnEvent(phase, text); eventText != "" {
					subscription.eventCh <- TurnStreamEvent{Text: eventText}
				}
			case "error":
				errorObject, _ := params["error"].(map[string]any)
				if errorObject != nil {
					errorMessage, _ = errorObject["message"].(string)
				}
			case "turn/completed":
				if turnObject, _ := params["turn"].(map[string]any); turnObject != nil && errorMessage == "" {
					if status, _ := turnObject["status"].(string); status == "failed" {
						if turnErr, _ := turnObject["error"].(map[string]any); turnErr != nil {
							errorMessage, _ = turnErr["message"].(string)
						}
					}
				}
				usage = extractTokenUsage(params)
				result := TurnResult{
					Text:             finalTurnText(completedMessages, deltas),
					ErrorMessage:     errorMessage,
					CommentaryText:   commentaryText(completedMessages),
					PlanText:         strings.TrimSpace(strings.Join(planDeltas, "")),
					ReasoningSummary: strings.TrimSpace(strings.Join(reasoningSummaryDeltas, "")),
					Usage:            usage,
				}
				logx.Debugf("codex turn completed %s", logx.KVSummary(
					"thread_id", subscription.threadID,
					"turn_id", subscription.turnID,
					"usage", summarizeTokenUsage(result.Usage),
					"final_text", logx.SummarizeText(result.Text),
					"commentary", logx.SummarizeText(result.CommentaryText),
					"plan", logx.SummarizeText(result.PlanText),
					"reasoning_summary", logx.SummarizeText(result.ReasoningSummary),
				))
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

func extractTokenUsage(params map[string]any) *TokenUsage {
	candidates := []map[string]any{params}
	if usage, ok := params["usage"].(map[string]any); ok {
		candidates = append(candidates, usage)
	}
	if turn, ok := params["turn"].(map[string]any); ok {
		candidates = append(candidates, turn)
		if usage, ok := turn["usage"].(map[string]any); ok {
			candidates = append(candidates, usage)
		}
	}

	var usage TokenUsage
	var foundInput bool
	var foundOutput bool
	var foundTotal bool

	for _, candidate := range candidates {
		if !foundInput {
			if value, ok := extractUsageCount(candidate, "inputTokens", "input_tokens", "promptTokens", "prompt_tokens"); ok {
				usage.Input = value
				foundInput = true
			}
		}
		if !foundOutput {
			if value, ok := extractUsageCount(candidate, "outputTokens", "output_tokens", "completionTokens", "completion_tokens"); ok {
				usage.Output = value
				foundOutput = true
			}
		}
		if !foundTotal {
			if value, ok := extractUsageCount(candidate, "totalTokens", "total_tokens"); ok {
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

func extractUsageCount(values map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		raw, ok := values[key]
		if !ok {
			continue
		}
		if number, ok := numberToInt64(raw); ok {
			return number, true
		}
	}
	return 0, false
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

func extractNestedString(object map[string]any, keys ...string) (string, error) {
	current := any(object)
	for _, key := range keys {
		next, ok := current.(map[string]any)
		if !ok {
			return "", fmt.Errorf("missing %s", strings.Join(keys, "."))
		}
		current = next[key]
	}
	value, ok := current.(string)
	if !ok || value == "" {
		return "", fmt.Errorf("missing %s", strings.Join(keys, "."))
	}
	return value, nil
}

func numberToInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		if typed < math.MinInt64 || typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case uint64:
		if typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	default:
		return 0, false
	}
}
