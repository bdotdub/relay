package codex

import (
	"strings"
	"testing"

	"github.com/bdotdub/relay/internal/config"
)

func TestExtractTokenUsageFromTurnUsage(t *testing.T) {
	input := int64(11)
	output := int64(7)
	total := int64(18)
	usage := extractTokenUsage(usageCarrier{
		Turn: &struct {
			Status string `json:"status"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error,omitempty"`
			Usage *protocolUsage `json:"usage,omitempty"`
		}{
			Usage: &protocolUsage{
				InputTokensSnake:  &input,
				OutputTokensSnake: &output,
				TotalTokensSnake:  &total,
			},
		},
	})
	if usage == nil {
		t.Fatal("expected usage")
	}
	if usage.Input != 11 || usage.Output != 7 || usage.Total != 18 {
		t.Fatalf("unexpected usage: %#v", usage)
	}
}

func TestExtractTokenUsageComputesTotalWhenMissing(t *testing.T) {
	input := int64(5)
	output := int64(9)
	usage := extractTokenUsage(usageCarrier{
		Usage: &protocolUsage{
			PromptTokens:     &input,
			CompletionTokens: &output,
		},
	})
	if usage == nil {
		t.Fatal("expected usage")
	}
	if usage.Input != 5 || usage.Output != 9 || usage.Total != 14 {
		t.Fatalf("unexpected usage: %#v", usage)
	}
}

func TestBaseThreadParamsYoloOverridesSandboxAndApproval(t *testing.T) {
	client := &Client{
		cfg: config.Config{
			CodexCWD:            "/tmp/project",
			CodexModel:          "gpt-5.4",
			CodexApprovalPolicy: "on-request",
			CodexSandbox:        "workspace-write",
			CodexPersonality:    "pragmatic",
		},
	}

	params := client.baseThreadParams(ThreadOptions{Yolo: true})

	if got := params["approvalPolicy"]; got != "never" {
		t.Fatalf("unexpected approval policy: %#v", got)
	}
	if got := params["sandbox"]; got != "danger-full-access" {
		t.Fatalf("unexpected sandbox: %#v", got)
	}
	if got := params["personality"]; got != "pragmatic" {
		t.Fatalf("unexpected personality: %#v", got)
	}
	if got := params["model"]; got != "gpt-5.4" {
		t.Fatalf("unexpected model: %#v", got)
	}
}

func TestMergedThreadConfigDropsPermissionProfileInYoloMode(t *testing.T) {
	client := &Client{
		cfg: config.Config{
			CodexConfig: map[string]any{
				"permission_profile": map[string]any{
					"network": map[string]any{"enabled": false},
				},
				"custom": "value",
			},
			CodexNetworkEnabled: "false",
			CodexFsReadPaths:    []string{"/tmp/read"},
		},
	}

	normal := client.mergedThreadConfig(ThreadOptions{})
	if _, ok := normal["permission_profile"]; !ok {
		t.Fatalf("expected permission profile in normal config: %#v", normal)
	}

	yolo := client.mergedThreadConfig(ThreadOptions{Yolo: true})
	if _, ok := yolo["permission_profile"]; ok {
		t.Fatalf("did not expect permission profile in yolo config: %#v", yolo)
	}
	if got := yolo["custom"]; got != "value" {
		t.Fatalf("expected custom config to be preserved, got %#v", got)
	}
}

func TestBaseThreadParamsUsesPerThreadModelOverride(t *testing.T) {
	client := &Client{
		cfg: config.Config{
			CodexCWD:   "/tmp/project",
			CodexModel: "gpt-5.4",
		},
	}

	params := client.baseThreadParams(ThreadOptions{Model: "gpt-5"})

	if got := params["model"]; got != "gpt-5" {
		t.Fatalf("unexpected model: %#v", got)
	}
}

func TestBaseThreadParamsUsesPerThreadServiceTierOverride(t *testing.T) {
	client := &Client{
		cfg: config.Config{
			CodexCWD:         "/tmp/project",
			CodexServiceTier: "fast",
		},
	}

	params := client.baseThreadParams(ThreadOptions{ServiceTierSet: true, ServiceTier: ""})
	if _, ok := params["serviceTier"]; ok {
		t.Fatalf("did not expect service tier when override clears it: %#v", params["serviceTier"])
	}

	params = client.baseThreadParams(ThreadOptions{ServiceTierSet: true, ServiceTier: "fast"})
	if got := params["serviceTier"]; got != "fast" {
		t.Fatalf("unexpected service tier: %#v", got)
	}
}

func TestBaseThreadParamsAppendsRelayDeveloperInstructions(t *testing.T) {
	client := &Client{
		cfg: config.Config{
			CodexCWD:                   "/tmp/project",
			CodexDeveloperInstructions: "Prefer concise answers.",
		},
	}

	params := client.baseThreadParams(ThreadOptions{})

	got, ok := params["developerInstructions"].(string)
	if !ok {
		t.Fatalf("expected developer instructions string, got %#v", params["developerInstructions"])
	}
	if !strings.Contains(got, "Prefer concise answers.") {
		t.Fatalf("expected configured developer instructions to be preserved, got %q", got)
	}
	if !strings.Contains(got, relayDeveloperInstructions) {
		t.Fatalf("expected relay developer instructions to be appended, got %q", got)
	}
}
