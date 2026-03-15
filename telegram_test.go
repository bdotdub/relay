package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestChunkMessageRespectsLimit(t *testing.T) {
	text := "Paragraph one.\n\n" + stringsRepeat("x", 50) + "\n" + stringsRepeat("y", 50)
	chunks := chunkMessage(text, 40)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for _, chunk := range chunks {
		if len(chunk) > 40 {
			t.Fatalf("chunk exceeded limit: %q", chunk)
		}
	}
}

func TestChunkMessageEmptyPlaceholder(t *testing.T) {
	chunks := chunkMessage("   ", 20)
	if len(chunks) != 1 || chunks[0] != "(empty response)" {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
}

func TestTelegramMarkdownV2FormatsHeadingsAndCode(t *testing.T) {
	text := "# Title\nUse `fmt.Println()` here.\n\n```go\nfmt.Println(\"hi\")\n```"
	formatted := telegramMarkdownV2(text)

	expected := "*Title*\nUse `fmt.Println()` here\\.\n\n```go\nfmt.Println(\"hi\")\n```"
	if formatted != expected {
		t.Fatalf("unexpected formatted markdown:\nwant: %q\ngot:  %q", expected, formatted)
	}
}

func TestTelegramClientSendMessageUsesMarkdownV2(t *testing.T) {
	var payload map[string]any
	client := &telegramClient{
		baseURL: "https://example.invalid",
		client: &http.Client{
			Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				defer request.Body.Close()
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":true}`)),
				}, nil
			}),
		},
	}
	if err := client.sendMessage(context.Background(), 7, "# Title"); err != nil {
		t.Fatalf("sendMessage: %v", err)
	}

	if got := payload["parse_mode"]; got != "MarkdownV2" {
		t.Fatalf("unexpected parse mode: %#v", got)
	}
	if got := payload["text"]; got != "*Title*" {
		t.Fatalf("unexpected formatted text: %#v", got)
	}
}

type roundTripFunc func(request *http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
