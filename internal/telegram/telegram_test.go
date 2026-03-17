package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestChunkMessageRespectsLimit(t *testing.T) {
	text := "Paragraph one.\n\n" + strings.Repeat("x", 50) + "\n" + strings.Repeat("y", 50)
	chunks := ChunkMessage(text, 40)
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
	chunks := ChunkMessage("   ", 20)
	if len(chunks) != 1 || chunks[0] != "(empty response)" {
		t.Fatalf("unexpected chunks: %#v", chunks)
	}
}

func TestTelegramMarkdownV2FormatsHeadingsAndCode(t *testing.T) {
	text := "# Title\nUse `fmt.Println()` here.\n\n```go\nfmt.Println(\"hi\")\n```"
	formatted := markdownV2(text)

	expected := "*Title*\nUse `fmt.Println()` here\\.\n\n```go\nfmt.Println(\"hi\")\n```"
	if formatted != expected {
		t.Fatalf("unexpected formatted markdown:\nwant: %q\ngot:  %q", expected, formatted)
	}
}

func TestTelegramMarkdownV2UsesNBSPBeforeInlineCode(t *testing.T) {
	text := "Executed: `designprompt-loader --style bauhaus`"
	formatted := markdownV2(text)

	expected := "Executed: `designprompt-loader --style bauhaus`"
	if formatted != expected {
		t.Fatalf("unexpected formatted markdown:\nwant: %q\ngot:  %q", expected, formatted)
	}
}

func TestTelegramMarkdownV2PreservesMarkdownLinks(t *testing.T) {
	text := "See [OpenAI](https://openai.com/docs) for details."
	formatted := markdownV2(text)

	expected := "See [OpenAI](https://openai.com/docs) for details\\."
	if formatted != expected {
		t.Fatalf("unexpected formatted markdown:\nwant: %q\ngot:  %q", expected, formatted)
	}
}

func TestTelegramMarkdownV2FormatsLists(t *testing.T) {
	text := "- first item\n* second item\n1. step one\n2) step two"
	formatted := markdownV2(text)

	expected := "• first item\n• second item\n1\\. step one\n2\\. step two"
	if formatted != expected {
		t.Fatalf("unexpected formatted markdown:\nwant: %q\ngot:  %q", expected, formatted)
	}
}

func TestTelegramMarkdownV2FormatsInlineEmphasis(t *testing.T) {
	text := "Use **bold** and _italic_ and keep *already telegram bold*."
	formatted := markdownV2(text)

	expected := "Use *bold* and _italic_ and keep *already telegram bold*\\."
	if formatted != expected {
		t.Fatalf("unexpected formatted markdown:\nwant: %q\ngot:  %q", expected, formatted)
	}
}

func TestTelegramClientSendMessageUsesMarkdownV2(t *testing.T) {
	var payload map[string]any
	client := &Client{
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
	if err := client.SendMessage(context.Background(), 7, "# Title"); err != nil {
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
