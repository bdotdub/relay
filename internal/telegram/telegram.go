package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/bdotdub/relay/internal/logx"
)

type Service interface {
	DeleteWebhook(ctx context.Context, dropPending bool) error
	GetUpdates(ctx context.Context, offset *int64, timeoutSeconds int) ([]Update, error)
	SendMessage(ctx context.Context, chatID int64, text string) error
	SendChatAction(ctx context.Context, chatID int64, action string) error
}

type Client struct {
	baseURL string
	client  *http.Client
}

type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

type Message struct {
	MessageID int64  `json:"message_id"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
}

type Chat struct {
	ID int64 `json:"id"`
}

type telegramResponse[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	Description string `json:"description"`
}

func NewClient(token string) *Client {
	return &Client{
		baseURL: fmt.Sprintf("https://api.telegram.org/bot%s", token),
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

func (c *Client) DeleteWebhook(ctx context.Context, dropPending bool) error {
	logx.Debugf("telegram deleteWebhook %s", logx.KVSummary("drop_pending", dropPending))
	payload := map[string]any{
		"drop_pending_updates": dropPending,
	}
	var result any
	return c.call(ctx, "deleteWebhook", payload, &result)
}

func (c *Client) GetUpdates(ctx context.Context, offset *int64, timeoutSeconds int) ([]Update, error) {
	logx.Debugf("telegram getUpdates %s", logx.KVSummary("offset", offsetValue(offset), "timeout", timeoutSeconds))
	payload := map[string]any{
		"timeout":         timeoutSeconds,
		"allowed_updates": []string{"message"},
	}
	if offset != nil {
		payload["offset"] = *offset
	}
	var result []Update
	if err := c.call(ctx, "getUpdates", payload, &result); err != nil {
		return nil, err
	}
	if len(result) > 0 {
		logx.Debugf("telegram getUpdates result %s", logx.KVSummary("updates", len(result)))
	}
	return result, nil
}

func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) error {
	logx.Debugf("telegram sendMessage %s", logx.KVSummary(
		"chat_id", chatID,
		"text", logx.SummarizeText(text),
	))
	payload := map[string]any{
		"chat_id":                  chatID,
		"text":                     markdownV2(text),
		"parse_mode":               "MarkdownV2",
		"disable_web_page_preview": true,
	}
	var result any
	return c.call(ctx, "sendMessage", payload, &result)
}

func (c *Client) SendChatAction(ctx context.Context, chatID int64, action string) error {
	logx.Debugf("telegram sendChatAction %s", logx.KVSummary("chat_id", chatID, "action", action))
	payload := map[string]any{
		"chat_id": chatID,
		"action":  action,
	}
	var result any
	return c.call(ctx, "sendChatAction", payload, &result)
}

func (c *Client) call(ctx context.Context, method string, payload any, out any) error {
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
	logx.Debugf("telegram %s ok", method)
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

func offsetValue(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func ChunkMessage(text string, limit int) []string {
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

var headingPattern = regexp.MustCompile(`^\s{0,3}#{1,6}\s+(.+?)\s*$`)
var unorderedListPattern = regexp.MustCompile(`^\s{0,3}[-*+]\s+(.+?)\s*$`)
var orderedListPattern = regexp.MustCompile(`^\s{0,3}(\d+)[.)]\s+(.+?)\s*$`)

func markdownV2(text string) string {
	var out strings.Builder
	for len(text) > 0 {
		if strings.HasPrefix(text, "```") {
			block, rest, ok := consumeFencedCodeBlock(text)
			if ok {
				out.WriteString(block)
				text = rest
				continue
			}
		}
		if strings.HasPrefix(text, "`") {
			inline, rest, ok := consumeInlineCode(text)
			if ok {
				out.WriteString(inline)
				text = rest
				continue
			}
		}
		if strings.HasPrefix(text, "[") {
			link, rest, ok := consumeLink(text)
			if ok {
				out.WriteString(link)
				text = rest
				continue
			}
			out.WriteString(escapeMarkdownV2(text[:1]))
			text = text[1:]
			continue
		}

		next := len(text)
		if index := strings.Index(text, "```"); index >= 0 && index < next {
			next = index
		}
		if index := strings.Index(text, "`"); index >= 0 && index < next {
			next = index
		}
		if index := strings.Index(text, "["); index >= 0 && index < next {
			next = index
		}
		out.WriteString(formatMarkdownPlain(text[:next]))
		text = text[next:]
	}
	return out.String()
}

func consumeFencedCodeBlock(text string) (string, string, bool) {
	if !strings.HasPrefix(text, "```") {
		return "", text, false
	}
	rest := text[3:]
	lineEnd := strings.Index(rest, "\n")
	if lineEnd < 0 {
		return escapeMarkdownV2(text), "", true
	}
	language := strings.TrimSpace(rest[:lineEnd])
	contentStart := 3 + lineEnd + 1
	closing := strings.Index(text[contentStart:], "```")
	if closing < 0 {
		return escapeMarkdownV2(text), "", true
	}
	contentEnd := contentStart + closing
	content := text[contentStart:contentEnd]
	var out strings.Builder
	out.WriteString("```")
	if language != "" {
		out.WriteString(language)
	}
	out.WriteByte('\n')
	out.WriteString(escapeCode(content))
	out.WriteString("```")
	return out.String(), text[contentEnd+3:], true
}

func consumeInlineCode(text string) (string, string, bool) {
	if !strings.HasPrefix(text, "`") {
		return "", text, false
	}
	end := strings.Index(text[1:], "`")
	if end < 0 {
		return escapeMarkdownV2(text[:1]), text[1:], true
	}
	content := text[1 : 1+end]
	if strings.Contains(content, "\n") {
		return escapeMarkdownV2(text[:1]), text[1:], true
	}
	return "`" + escapeCode(content) + "`", text[1+end+1:], true
}

func consumeLink(text string) (string, string, bool) {
	if !strings.HasPrefix(text, "[") {
		return "", text, false
	}
	labelEnd := strings.Index(text, "](")
	if labelEnd <= 0 {
		return "", text, false
	}
	urlStart := labelEnd + 2
	urlEnd := strings.Index(text[urlStart:], ")")
	if urlEnd < 0 {
		return "", text, false
	}
	urlEnd += urlStart
	label := text[1:labelEnd]
	url := text[urlStart:urlEnd]
	if strings.Contains(label, "\n") || strings.TrimSpace(label) == "" {
		return "", text, false
	}
	if strings.Contains(url, "\n") || strings.TrimSpace(url) == "" {
		return "", text, false
	}
	return "[" + escapeMarkdownV2(label) + "](" + escapeLinkURL(url) + ")", text[urlEnd+1:], true
}

func formatMarkdownPlain(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		matches := headingPattern.FindStringSubmatch(line)
		if len(matches) == 2 {
			lines[i] = "*" + escapeMarkdownV2(matches[1]) + "*"
			continue
		}
		matches = unorderedListPattern.FindStringSubmatch(line)
		if len(matches) == 2 {
			lines[i] = "• " + escapeMarkdownV2(matches[1])
			continue
		}
		matches = orderedListPattern.FindStringSubmatch(line)
		if len(matches) == 3 {
			lines[i] = matches[1] + "\\. " + escapeMarkdownV2(matches[2])
			continue
		}
		lines[i] = escapeMarkdownV2(line)
	}
	return strings.Join(lines, "\n")
}

func escapeMarkdownV2(text string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"_", "\\_",
		"*", "\\*",
		"[", "\\[",
		"]", "\\]",
		"(", "\\(",
		")", "\\)",
		"~", "\\~",
		"`", "\\`",
		">", "\\>",
		"#", "\\#",
		"+", "\\+",
		"-", "\\-",
		"=", "\\=",
		"|", "\\|",
		"{", "\\{",
		"}", "\\}",
		".", "\\.",
		"!", "\\!",
	)
	return replacer.Replace(text)
}

func escapeCode(text string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"`", "\\`",
	)
	return replacer.Replace(text)
}

func escapeLinkURL(text string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		")", "\\)",
	)
	return replacer.Replace(text)
}
