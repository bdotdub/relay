package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

type codexService interface {
	close() error
	newThread(ctx context.Context, options codexThreadOptions) (string, error)
	ensureThread(ctx context.Context, threadID string, options codexThreadOptions) (string, error)
	startTurn(ctx context.Context, threadID string, userText string) (*codexTurnHandle, error)
	steerTurn(ctx context.Context, threadID string, turnID string, userText string) error
}

type codexThreadOptions struct {
	yolo  bool
	model string
}

type codexClient struct {
	rpc           *jsonRPCClient
	cfg           config
	loadedThreads map[string]struct{}
	loadedMu      sync.Mutex
	activeTurns   map[string]*codexTurnSubscription
	activeTurnsMu sync.Mutex
}

type codexTurnHandle struct {
	ThreadID string
	TurnID   string
	EventCh  <-chan turnStreamEvent
	ResultCh <-chan turnResult
}

type codexTurnSubscription struct {
	threadID      string
	turnID        string
	notifications chan map[string]any
	eventCh       chan turnStreamEvent
	resultCh      chan turnResult
	stopCh        chan error
}

type turnStreamEvent struct {
	text string
}

type turnResult struct {
	text             string
	errorMessage     string
	commentaryText   string
	planText         string
	reasoningSummary string
	usage            *tokenUsage
	err              error
}

type tokenUsage struct {
	input  int64
	output int64
	total  int64
}

const relayDeveloperInstructions = "This Codex session is relayed through Telegram, and the user interacts with it there. Telegram messages are rendered with MarkdownV2. When you include a link, prefer the Markdown link form \"[label](url)\" so it renders correctly in Telegram. Do not include local filesystem paths unless they are truly necessary, because the user is interacting through Telegram rather than a shared local workspace."

func newCodexClient(cfg config) (*codexClient, error) {
	var rpc *jsonRPCClient
	var err error
	if cfg.codexStartAppServer {
		rpc, err = newJSONRPCStdioClient(cfg.codexAppServerCommand)
	} else {
		rpc, err = newJSONRPCWebSocketClient(cfg.codexAppServerWSURL)
	}
	if err != nil {
		return nil, err
	}

	client := &codexClient{
		rpc:           rpc,
		cfg:           cfg,
		loadedThreads: make(map[string]struct{}),
		activeTurns:   make(map[string]*codexTurnSubscription),
	}
	if err := client.initialize(context.Background()); err != nil {
		_ = rpc.close()
		return nil, err
	}
	verbosef("codex client ready")
	go client.dispatchNotifications()
	return client, nil
}

func (c *codexClient) close() error {
	if c.rpc == nil {
		return nil
	}
	return c.rpc.close()
}

func (c *codexClient) initialize(ctx context.Context) error {
	_, err := c.rpc.request(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "telegram-codex-relay",
			"version": "0.1.0",
		},
	})
	if err != nil {
		return err
	}
	return c.rpc.notify("initialized", nil)
}

func (c *codexClient) newThread(ctx context.Context, options codexThreadOptions) (string, error) {
	response, err := c.rpc.request(ctx, "thread/start", c.newThreadParams(options))
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
	verbosef("codex thread started %s", kvSummary("thread_id", threadID))
	return threadID, nil
}

func (c *codexClient) ensureThread(ctx context.Context, threadID string, options codexThreadOptions) (string, error) {
	if threadID == "" {
		return c.newThread(ctx, options)
	}
	c.loadedMu.Lock()
	_, ok := c.loadedThreads[threadID]
	c.loadedMu.Unlock()
	if ok {
		return threadID, nil
	}

	params := c.resumeThreadParams(options)
	params["threadId"] = threadID
	if _, err := c.rpc.request(ctx, "thread/resume", params); err == nil {
		c.loadedMu.Lock()
		c.loadedThreads[threadID] = struct{}{}
		c.loadedMu.Unlock()
		verbosef("codex thread resumed %s", kvSummary("thread_id", threadID))
		return threadID, nil
	}
	verbosef("codex thread resume failed; starting new thread %s", kvSummary("thread_id", threadID))
	return c.newThread(ctx, options)
}

func (c *codexClient) startTurn(ctx context.Context, threadID string, userText string) (*codexTurnHandle, error) {
	subscription := &codexTurnSubscription{
		threadID:      threadID,
		notifications: make(chan map[string]any, 128),
		eventCh:       make(chan turnStreamEvent, 32),
		resultCh:      make(chan turnResult, 1),
		stopCh:        make(chan error, 1),
	}

	c.activeTurnsMu.Lock()
	if _, exists := c.activeTurns[threadID]; exists {
		c.activeTurnsMu.Unlock()
		return nil, fmt.Errorf("thread %s already has an active turn", threadID)
	}
	c.activeTurns[threadID] = subscription
	c.activeTurnsMu.Unlock()

	response, err := c.rpc.request(ctx, "turn/start", map[string]any{
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
	verbosef("codex turn started %s", kvSummary("thread_id", threadID, "turn_id", turnID, "text", summarizeText(userText)))
	go c.collectTurnResult(subscription)

	return &codexTurnHandle{
		ThreadID: threadID,
		TurnID:   turnID,
		EventCh:  subscription.eventCh,
		ResultCh: subscription.resultCh,
	}, nil
}

func (c *codexClient) steerTurn(ctx context.Context, threadID string, turnID string, userText string) error {
	verbosef("codex turn steer %s", kvSummary("thread_id", threadID, "turn_id", turnID, "text", summarizeText(userText)))
	_, err := c.rpc.request(ctx, "turn/steer", map[string]any{
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

func (c *codexClient) dispatchNotifications() {
	for {
		notification, err := c.rpc.nextNotification(context.Background())
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
		verbosef("codex notification routed %s", kvSummary("method", method, "thread_id", threadID, "turn_id", turnID))

		select {
		case subscription.notifications <- notification:
		default:
			subscription.notifications <- notification
		}
	}
}

func (c *codexClient) collectTurnResult(subscription *codexTurnSubscription) {
	defer c.removeActiveTurn(subscription.threadID)
	defer close(subscription.eventCh)

	var deltas string
	var completedMessages []agentMessage
	var errorMessage string
	var planDeltas []string
	var reasoningSummaryDeltas []string
	var usage *tokenUsage

	for {
		select {
		case err := <-subscription.stopCh:
			subscription.resultCh <- turnResult{err: err}
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
				if stringsTrimSpace(delta) != "" {
					planDeltas = append(planDeltas, delta)
				}
			case "item/reasoning/summaryTextDelta":
				delta, _ := params["delta"].(string)
				if stringsTrimSpace(delta) != "" {
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
					subscription.eventCh <- turnStreamEvent{text: eventText}
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
				result := turnResult{
					text:             finalTurnText(completedMessages, deltas),
					errorMessage:     errorMessage,
					commentaryText:   commentaryText(completedMessages),
					planText:         stringsTrimSpace(strings.Join(planDeltas, "")),
					reasoningSummary: stringsTrimSpace(strings.Join(reasoningSummaryDeltas, "")),
					usage:            usage,
				}
				verbosef("codex turn completed %s", kvSummary(
					"thread_id", subscription.threadID,
					"turn_id", subscription.turnID,
					"usage", summarizeTokenUsage(result.usage),
					"final_text", summarizeText(result.text),
					"commentary", summarizeText(result.commentaryText),
					"plan", summarizeText(result.planText),
					"reasoning_summary", summarizeText(result.reasoningSummary),
				))
				subscription.resultCh <- result
				close(subscription.resultCh)
				return
			}
		}
	}
}

func (c *codexClient) removeActiveTurn(threadID string) {
	c.activeTurnsMu.Lock()
	delete(c.activeTurns, threadID)
	c.activeTurnsMu.Unlock()
}

func (c *codexClient) failActiveTurns(err error) {
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
		text := stringsTrimSpace(message.text)
		if text == "" {
			continue
		}
		all = append(all, text)
		if message.phase == "final_answer" {
			finals = append(finals, text)
		}
	}
	if len(finals) > 0 {
		return joinParagraphs(finals)
	}
	if len(all) > 0 {
		return joinParagraphs(all)
	}
	return stringsTrimSpace(deltas)
}

func commentaryText(messages []agentMessage) string {
	var commentary []string
	for _, message := range messages {
		if message.phase != "commentary" {
			continue
		}
		text := stringsTrimSpace(message.text)
		if text == "" {
			continue
		}
		commentary = append(commentary, text)
	}
	if len(commentary) == 0 {
		return ""
	}
	return joinParagraphs(commentary)
}

func formatIntermediateTurnEvent(phase string, text string) string {
	text = stringsTrimSpace(text)
	if text == "" || phase == "final_answer" {
		return ""
	}
	return text
}

func summarizeTokenUsage(usage *tokenUsage) string {
	if usage == nil {
		return "n/a"
	}
	parts := []string{}
	if usage.input > 0 || usage.output > 0 {
		parts = append(parts, fmt.Sprintf("input=%d", usage.input))
		parts = append(parts, fmt.Sprintf("output=%d", usage.output))
	}
	if usage.total > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("total=%d", usage.total))
	}
	return stringsJoin(parts, " ")
}

func extractTokenUsage(params map[string]any) *tokenUsage {
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

	var usage tokenUsage
	var foundInput bool
	var foundOutput bool
	var foundTotal bool

	for _, candidate := range candidates {
		if !foundInput {
			if value, ok := extractUsageCount(candidate, "inputTokens", "input_tokens", "promptTokens", "prompt_tokens"); ok {
				usage.input = value
				foundInput = true
			}
		}
		if !foundOutput {
			if value, ok := extractUsageCount(candidate, "outputTokens", "output_tokens", "completionTokens", "completion_tokens"); ok {
				usage.output = value
				foundOutput = true
			}
		}
		if !foundTotal {
			if value, ok := extractUsageCount(candidate, "totalTokens", "total_tokens"); ok {
				usage.total = value
				foundTotal = true
			}
		}
	}

	if !foundTotal && foundInput && foundOutput {
		usage.total = usage.input + usage.output
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

func (c *codexClient) baseThreadParams(options codexThreadOptions) map[string]any {
	params := map[string]any{
		"cwd": c.cfg.codexCWD,
	}
	if options.yolo {
		params["approvalPolicy"] = "never"
		params["sandbox"] = "danger-full-access"
	} else {
		insertOptionalString(params, "approvalPolicy", c.cfg.codexApprovalPolicy)
		insertOptionalString(params, "sandbox", c.cfg.codexSandbox)
	}
	insertOptionalString(params, "model", c.modelForOptions(options))
	insertOptionalString(params, "personality", c.cfg.codexPersonality)
	insertOptionalString(params, "serviceTier", c.cfg.codexServiceTier)
	insertOptionalString(params, "baseInstructions", c.cfg.codexBaseInstructions)
	insertOptionalString(params, "developerInstructions", c.developerInstructions())
	if merged := c.mergedThreadConfig(options); len(merged) > 0 {
		params["config"] = merged
	}
	return params
}

func (c *codexClient) developerInstructions() string {
	parts := make([]string, 0, 2)
	if value := strings.TrimSpace(c.cfg.codexDeveloperInstructions); value != "" {
		parts = append(parts, value)
	}
	parts = append(parts, relayDeveloperInstructions)
	return strings.Join(parts, "\n\n")
}

func (c *codexClient) modelForOptions(options codexThreadOptions) string {
	if stringsTrimSpace(options.model) != "" {
		return options.model
	}
	return c.cfg.codexModel
}

func (c *codexClient) newThreadParams(options codexThreadOptions) map[string]any {
	params := c.baseThreadParams(options)
	params["ephemeral"] = c.cfg.codexEphemeralThreads
	return params
}

func (c *codexClient) resumeThreadParams(options codexThreadOptions) map[string]any {
	return c.baseThreadParams(options)
}

// mergedThreadConfig returns the thread config map, merging in a permission profile when
// codexNetworkEnabled, codexFsReadPaths, or codexFsWritePaths are set. Uses snake_case keys
// to match the Codex protocol (file_system, network, permission_profile).
func (c *codexClient) mergedThreadConfig(options codexThreadOptions) map[string]any {
	merged := make(map[string]any)
	if c.cfg.codexConfig != nil {
		for k, v := range c.cfg.codexConfig {
			if options.yolo && k == "permission_profile" {
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

func (c *codexClient) buildPermissionProfile(options codexThreadOptions) map[string]any {
	if options.yolo {
		return nil
	}
	var network map[string]any
	switch strings.TrimSpace(strings.ToLower(c.cfg.codexNetworkEnabled)) {
	case "true", "1", "yes":
		network = map[string]any{"enabled": true}
	case "false", "0", "no":
		network = map[string]any{"enabled": false}
	}
	hasFS := len(c.cfg.codexFsReadPaths) > 0 || len(c.cfg.codexFsWritePaths) > 0
	var fileSystem map[string]any
	if hasFS {
		fileSystem = make(map[string]any)
		if len(c.cfg.codexFsReadPaths) > 0 {
			fileSystem["read"] = c.cfg.codexFsReadPaths
		}
		if len(c.cfg.codexFsWritePaths) > 0 {
			fileSystem["write"] = c.cfg.codexFsWritePaths
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
	if stringsTrimSpace(value) != "" {
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
