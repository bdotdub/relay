package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type codexClient struct {
	rpc           *jsonRPCClient
	cfg           config
	loadedThreads map[string]struct{}
}

type turnResult struct {
	text         string
	errorMessage string
}

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
	}
	if err := client.initialize(context.Background()); err != nil {
		_ = rpc.close()
		return nil, err
	}
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

func (c *codexClient) newThread(ctx context.Context) (string, error) {
	response, err := c.rpc.request(ctx, "thread/start", c.newThreadParams())
	if err != nil {
		return "", err
	}
	threadID, err := extractNestedString(response, "thread", "id")
	if err != nil {
		return "", err
	}
	c.loadedThreads[threadID] = struct{}{}
	return threadID, nil
}

func (c *codexClient) ensureThread(ctx context.Context, threadID string) (string, error) {
	if threadID == "" {
		return c.newThread(ctx)
	}
	if _, ok := c.loadedThreads[threadID]; ok {
		return threadID, nil
	}

	params := c.resumeThreadParams()
	params["threadId"] = threadID
	if _, err := c.rpc.request(ctx, "thread/resume", params); err == nil {
		c.loadedThreads[threadID] = struct{}{}
		return threadID, nil
	}
	return c.newThread(ctx)
}

func (c *codexClient) runTurn(ctx context.Context, threadID string, userText string) (turnResult, error) {
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
		return turnResult{}, err
	}
	turnID, err := extractNestedString(response, "turn", "id")
	if err != nil {
		return turnResult{}, err
	}

	var deltas string
	var completedMessages []agentMessage
	var errorMessage string
	completed := false

	for {
		notification, err := c.rpc.nextNotification(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return turnResult{}, err
			}
			return turnResult{}, fmt.Errorf("codex app server closed before turn completed: %w", err)
		}

		params, _ := notification["params"].(map[string]any)
		if params == nil {
			continue
		}
		notificationThreadID, _ := params["threadId"].(string)
		if notificationThreadID != threadID {
			continue
		}
		if notificationTurnID, ok := params["turnId"].(string); ok && notificationTurnID != turnID {
			continue
		}

		method, _ := notification["method"].(string)
		switch method {
		case "item/agentMessage/delta":
			delta, _ := params["delta"].(string)
			deltas += delta
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
		case "error":
			errorObject, _ := params["error"].(map[string]any)
			if errorObject != nil {
				errorMessage, _ = errorObject["message"].(string)
			}
		case "turn/completed":
			completed = true
		}

		if completed {
			break
		}
	}

	text := finalTurnText(completedMessages, deltas)
	return turnResult{
		text:         text,
		errorMessage: errorMessage,
	}, nil
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

func (c *codexClient) baseThreadParams() map[string]any {
	params := map[string]any{
		"cwd": c.cfg.codexCWD,
	}
	insertOptionalString(params, "approvalPolicy", c.cfg.codexApprovalPolicy)
	insertOptionalString(params, "sandbox", c.cfg.codexSandbox)
	insertOptionalString(params, "model", c.cfg.codexModel)
	insertOptionalString(params, "personality", c.cfg.codexPersonality)
	insertOptionalString(params, "serviceTier", c.cfg.codexServiceTier)
	insertOptionalString(params, "baseInstructions", c.cfg.codexBaseInstructions)
	insertOptionalString(params, "developerInstructions", c.cfg.codexDeveloperInstructions)
	if c.cfg.codexConfig != nil {
		params["config"] = c.cfg.codexConfig
	}
	return params
}

func (c *codexClient) newThreadParams() map[string]any {
	params := c.baseThreadParams()
	params["ephemeral"] = c.cfg.codexEphemeralThreads
	return params
}

func (c *codexClient) resumeThreadParams() map[string]any {
	return c.baseThreadParams()
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
