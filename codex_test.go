package main

import "testing"

func TestExtractTokenUsageFromTurnUsage(t *testing.T) {
	usage := extractTokenUsage(map[string]any{
		"turn": map[string]any{
			"usage": map[string]any{
				"input_tokens":  float64(11),
				"output_tokens": float64(7),
				"total_tokens":  float64(18),
			},
		},
	})
	if usage == nil {
		t.Fatal("expected usage")
	}
	if usage.input != 11 || usage.output != 7 || usage.total != 18 {
		t.Fatalf("unexpected usage: %#v", usage)
	}
}

func TestExtractTokenUsageComputesTotalWhenMissing(t *testing.T) {
	usage := extractTokenUsage(map[string]any{
		"usage": map[string]any{
			"promptTokens":     float64(5),
			"completionTokens": float64(9),
		},
	})
	if usage == nil {
		t.Fatal("expected usage")
	}
	if usage.input != 5 || usage.output != 9 || usage.total != 14 {
		t.Fatalf("unexpected usage: %#v", usage)
	}
}
