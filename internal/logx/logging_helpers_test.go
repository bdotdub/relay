package logx

import (
	"strings"
	"testing"
)

func TestSummarizeTextRedactsContent(t *testing.T) {
	summary := SummarizeText("top secret value")
	if strings.Contains(summary, "secret") {
		t.Fatalf("summary leaked source text: %q", summary)
	}
	if summary != "[redacted 16 chars]" {
		t.Fatalf("unexpected summary: %q", summary)
	}
}
