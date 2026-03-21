package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bdotdub/relay/internal/logx"
)

type Service interface {
	DeleteWebhook(ctx context.Context, dropPending bool) error
	GetUpdates(ctx context.Context, offset *int64, timeoutSeconds int) ([]Update, error)
	// DownloadFile fetches the image bytes for a Telegram file ID. It also
	// returns the file extension (e.g. ".jpg") derived from Telegram's file path,
	// which callers can use when writing the data to a local file.
	DownloadFile(ctx context.Context, fileID string) (data []byte, ext string, err error)
	SendMessage(ctx context.Context, chatID int64, text string) error
	SendChatAction(ctx context.Context, chatID int64, action string) error
	SetMyCommands(ctx context.Context, commands []BotCommand) error
}

type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

type Client struct {
	baseURL     string
	fileBaseURL string
	client      *http.Client
}

type RequestError struct {
	Method      string
	StatusCode  int
	Status      string
	Description string
	Err         error
}

func (e *RequestError) Error() string {
	if e == nil {
		return ""
	}
	if e.Status != "" {
		return fmt.Sprintf("telegram request %s returned HTTP %s", e.Method, e.Status)
	}
	if e.Description != "" {
		return fmt.Sprintf("telegram request %s failed: %s", e.Method, e.Description)
	}
	if e.Err != nil {
		return fmt.Sprintf("telegram request %s failed: %v", e.Method, e.Err)
	}
	return fmt.Sprintf("telegram request %s failed", e.Method)
}

func (e *RequestError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

type PhotoSize struct {
	FileID string `json:"file_id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

type Message struct {
	MessageID      int64       `json:"message_id"`
	Chat           Chat        `json:"chat"`
	Text           string      `json:"text"`
	Caption        string      `json:"caption"`
	Photo          []PhotoSize `json:"photo"`
	ReplyToMessage *Message    `json:"reply_to_message,omitempty"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type telegramResponse[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	Description string `json:"description"`
}

func NewClient(token string) *Client {
	return &Client{
		baseURL:     fmt.Sprintf("https://api.telegram.org/bot%s", token),
		fileBaseURL: fmt.Sprintf("https://api.telegram.org/file/bot%s", token),
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

func (c *Client) DeleteWebhook(ctx context.Context, dropPending bool) error {
	logx.Debug("telegram deleteWebhook", "drop_pending", dropPending)
	payload := map[string]any{
		"drop_pending_updates": dropPending,
	}
	var result any
	return c.call(ctx, "deleteWebhook", payload, &result)
}

// DownloadFile fetches the bytes for a Telegram file ID. The returned ext is
// the file extension (e.g. ".jpg") from Telegram's file path; callers should
// use it when writing the data to disk. The bot-token-bearing download URL is
// used internally and is never returned to callers.
func (c *Client) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	logx.Debug("telegram getFile", "file_id", fileID)
	var result struct {
		FilePath string `json:"file_path"`
	}
	if err := c.call(ctx, "getFile", map[string]any{"file_id": fileID}, &result); err != nil {
		return nil, "", err
	}
	if result.FilePath == "" {
		return nil, "", fmt.Errorf("getFile returned empty file_path for file_id %s", fileID)
	}
	ext := filepath.Ext(result.FilePath) // e.g. ".jpg"
	downloadURL := c.fileBaseURL + "/" + result.FilePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create download request for file_id %s: %w", fileID, err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("download telegram file %s: %w", fileID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("download telegram file %s returned HTTP %s", fileID, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read telegram file %s: %w", fileID, err)
	}
	logx.Debug("telegram file downloaded", "file_id", fileID, "bytes", len(data), "ext", ext)
	return data, ext, nil
}

func (c *Client) GetUpdates(ctx context.Context, offset *int64, timeoutSeconds int) ([]Update, error) {
	logx.Debug("telegram getUpdates", "offset", offsetValue(offset), "timeout", timeoutSeconds)
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
		logx.Debug("telegram getUpdates result", "updates", len(result))
	}
	return result, nil
}

func (c *Client) SendMessage(ctx context.Context, chatID int64, text string) error {
	logx.Debug("telegram sendMessage",
		"chat_id", chatID,
		"text", logx.SummarizeText(text),
	)
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
	logx.Debug("telegram sendChatAction", "chat_id", chatID, "action", action)
	payload := map[string]any{
		"chat_id": chatID,
		"action":  action,
	}
	var result any
	return c.call(ctx, "sendChatAction", payload, &result)
}

func (c *Client) SetMyCommands(ctx context.Context, commands []BotCommand) error {
	logx.Debug("telegram setMyCommands", "count", len(commands))
	payload := map[string]any{
		"commands": commands,
	}
	var result any
	return c.call(ctx, "setMyCommands", payload, &result)
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
		return &RequestError{
			Method: method,
			Err:    err,
		}
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &RequestError{
			Method:     method,
			StatusCode: response.StatusCode,
			Status:     response.Status,
		}
	}

	var decoded telegramResponse[json.RawMessage]
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return fmt.Errorf("decode telegram response %s: %w", method, err)
	}
	if !decoded.OK {
		if decoded.Description == "" {
			decoded.Description = "telegram API returned an error"
		}
		return &RequestError{
			Method:      method,
			Description: decoded.Description,
		}
	}
	logx.Debug("telegram request ok", "method", method)
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
			lines[i] = "*" + formatInlineMarkdown(matches[1]) + "*"
			continue
		}
		matches = unorderedListPattern.FindStringSubmatch(line)
		if len(matches) == 2 {
			lines[i] = "• " + formatInlineMarkdown(matches[1])
			continue
		}
		matches = orderedListPattern.FindStringSubmatch(line)
		if len(matches) == 3 {
			lines[i] = matches[1] + "\\. " + formatInlineMarkdown(matches[2])
			continue
		}
		lines[i] = formatInlineMarkdown(line)
	}
	return strings.Join(lines, "\n")
}

func formatInlineMarkdown(text string) string {
	var out strings.Builder
	for len(text) > 0 {
		if strings.HasPrefix(text, "**") {
			bold, rest, ok := consumeInlineStyle(text, "**", "*")
			if ok {
				out.WriteString(bold)
				text = rest
				continue
			}
		}
		if strings.HasPrefix(text, "_") {
			italic, rest, ok := consumeInlineStyle(text, "_", "_")
			if ok {
				out.WriteString(italic)
				text = rest
				continue
			}
		}
		if strings.HasPrefix(text, "*") && !strings.HasPrefix(text, "**") {
			bold, rest, ok := consumeInlineStyle(text, "*", "*")
			if ok {
				out.WriteString(bold)
				text = rest
				continue
			}
		}

		out.WriteString(escapeMarkdownV2(text[:1]))
		text = text[1:]
	}
	return out.String()
}

func consumeInlineStyle(text, marker, replacement string) (string, string, bool) {
	if !strings.HasPrefix(text, marker) {
		return "", text, false
	}

	end := strings.Index(text[len(marker):], marker)
	if end < 0 {
		return "", text, false
	}
	end += len(marker)

	content := text[len(marker):end]
	if strings.Contains(content, "\n") || strings.TrimSpace(content) == "" {
		return "", text, false
	}
	if strings.HasPrefix(content, " ") || strings.HasSuffix(content, " ") {
		return "", text, false
	}

	return replacement + formatInlineMarkdown(content) + replacement, text[end+len(marker):], true
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
