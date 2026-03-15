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

func TestBaseThreadParamsYoloOverridesSandboxAndApproval(t *testing.T) {
	client := &codexClient{
		cfg: config{
			codexCWD:            "/tmp/project",
			codexModel:          "gpt-5.3-codex-spark",
			codexApprovalPolicy: "on-request",
			codexSandbox:        "workspace-write",
			codexPersonality:    "pragmatic",
		},
	}

	params := client.baseThreadParams(codexThreadOptions{yolo: true})

	if got := params["approvalPolicy"]; got != "never" {
		t.Fatalf("unexpected approval policy: %#v", got)
	}
	if got := params["sandbox"]; got != "danger-full-access" {
		t.Fatalf("unexpected sandbox: %#v", got)
	}
	if got := params["personality"]; got != "pragmatic" {
		t.Fatalf("unexpected personality: %#v", got)
	}
	if got := params["model"]; got != "gpt-5.3-codex-spark" {
		t.Fatalf("unexpected model: %#v", got)
	}
}

func TestMergedThreadConfigDropsPermissionProfileInYoloMode(t *testing.T) {
	client := &codexClient{
		cfg: config{
			codexConfig: map[string]any{
				"permission_profile": map[string]any{
					"network": map[string]any{"enabled": false},
				},
				"custom": "value",
			},
			codexNetworkEnabled: "false",
			codexFsReadPaths:    []string{"/tmp/read"},
		},
	}

	normal := client.mergedThreadConfig(codexThreadOptions{})
	if _, ok := normal["permission_profile"]; !ok {
		t.Fatalf("expected permission profile in normal config: %#v", normal)
	}

	yolo := client.mergedThreadConfig(codexThreadOptions{yolo: true})
	if _, ok := yolo["permission_profile"]; ok {
		t.Fatalf("did not expect permission profile in yolo config: %#v", yolo)
	}
	if got := yolo["custom"]; got != "value" {
		t.Fatalf("expected custom config to be preserved, got %#v", got)
	}
}

func TestBaseThreadParamsUsesPerThreadModelOverride(t *testing.T) {
	client := &codexClient{
		cfg: config{
			codexCWD:   "/tmp/project",
			codexModel: "gpt-5.3-codex-spark",
		},
	}

	params := client.baseThreadParams(codexThreadOptions{model: "gpt-5"})

	if got := params["model"]; got != "gpt-5" {
		t.Fatalf("unexpected model: %#v", got)
	}
}
