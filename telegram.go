package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type telegramClient struct {
	baseURL string
	client  *http.Client
}

type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
}

type telegramMessage struct {
	MessageID int64        `json:"message_id"`
	Chat      telegramChat `json:"chat"`
	Text      string       `json:"text"`
}

type telegramChat struct {
	ID int64 `json:"id"`
}

type telegramResponse[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	Description string `json:"description"`
}

func newTelegramClient(token string) *telegramClient {
	return &telegramClient{
		baseURL: fmt.Sprintf("https://api.telegram.org/bot%s", token),
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

func (c *telegramClient) deleteWebhook(ctx context.Context, dropPending bool) error {
	payload := map[string]any{
		"drop_pending_updates": dropPending,
	}
	var result any
	return c.call(ctx, "deleteWebhook", payload, &result)
}

func (c *telegramClient) getUpdates(ctx context.Context, offset *int64, timeoutSeconds int) ([]telegramUpdate, error) {
	payload := map[string]any{
		"timeout":         timeoutSeconds,
		"allowed_updates": []string{"message"},
	}
	if offset != nil {
		payload["offset"] = *offset
	}
	var result []telegramUpdate
	if err := c.call(ctx, "getUpdates", payload, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *telegramClient) sendMessage(ctx context.Context, chatID int64, text string, replyToMessageID *int64) error {
	payload := map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}
	if replyToMessageID != nil {
		payload["reply_to_message_id"] = *replyToMessageID
	}
	var result any
	return c.call(ctx, "sendMessage", payload, &result)
}

func (c *telegramClient) call(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal telegram request %s: %w", method, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create telegram request %s: %w", method, err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("telegram request %s failed: %w", method, err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("telegram request %s returned HTTP %s", method, response.Status)
	}

	var decoded telegramResponse[json.RawMessage]
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return fmt.Errorf("decode telegram response %s: %w", method, err)
	}
	if !decoded.OK {
		if decoded.Description == "" {
			decoded.Description = "telegram API returned an error"
		}
		return fmt.Errorf("telegram request %s failed: %s", method, decoded.Description)
	}
	if out == nil {
		return nil
	}
	if len(decoded.Result) == 0 || string(decoded.Result) == "null" {
		return nil
	}
	if err := json.Unmarshal(decoded.Result, out); err != nil {
		return fmt.Errorf("decode telegram result %s: %w", method, err)
	}
	return nil
}

func chunkMessage(text string, limit int) []string {
	stripped := strings.TrimSpace(text)
	if stripped == "" || limit <= 0 {
		return []string{"(empty response)"}
	}
	if len(stripped) <= limit {
		return []string{stripped}
	}

	var chunks []string
	current := ""
	for _, paragraph := range strings.Split(stripped, "\n\n") {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" {
			continue
		}
		joined := paragraph
		if current != "" {
			joined = current + "\n\n" + paragraph
		}
		if len(joined) <= limit {
			current = joined
			continue
		}
		if current != "" {
			chunks = append(chunks, current)
			current = ""
		}
		if len(paragraph) <= limit {
			current = paragraph
			continue
		}
		for _, line := range strings.Split(paragraph, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			joinedLine := line
			if current != "" {
				joinedLine = current + "\n" + line
			}
			if len(joinedLine) <= limit {
				current = joinedLine
				continue
			}
			if current != "" {
				chunks = append(chunks, current)
				current = ""
			}
			for len(line) > limit {
				splitAt := previousCharBoundary(line, limit)
				chunks = append(chunks, line[:splitAt])
				line = line[splitAt:]
			}
			current = line
		}
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	return chunks
}

func previousCharBoundary(text string, maxBytes int) int {
	if maxBytes >= len(text) {
		return len(text)
	}
	index := maxBytes
	for index > 0 && (text[index]&0xc0) == 0x80 {
		index--
	}
	if index == 0 {
		return maxBytes
	}
	return index
}
