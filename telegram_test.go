package main

import "testing"

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
