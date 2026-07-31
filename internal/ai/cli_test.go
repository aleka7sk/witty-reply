package ai

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

func TestParseCLIResultUsesStructuredOutputAndUsage(t *testing.T) {
	structured, _ := json.Marshal(validMixedResult())
	envelope := map[string]any{
		"subtype":           "success",
		"is_error":          false,
		"result":            "ignored free-form text",
		"structured_output": json.RawMessage(structured),
		"modelUsage": map[string]any{
			"claude-sonnet-5-20260701": map[string]any{
				"inputTokens": 11, "outputTokens": 7,
				"cacheCreationInputTokens": 3, "cacheReadInputTokens": 5,
			},
		},
	}
	raw, _ := json.Marshal(envelope)
	result, err := parseCLIResult(raw, domain.ToneMix, defaultModel)
	if err != nil {
		t.Fatalf("parseCLIResult() error = %v", err)
	}
	if result.Provider != providerClaudeCLI || result.Model != "claude-sonnet-5-20260701" {
		t.Fatalf("metadata = provider %q model %q", result.Provider, result.Model)
	}
	if result.Usage.InputTokens != 19 || result.Usage.OutputTokens != 7 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestClaudeCLIArgumentsAndEnvironmentAreConstrained(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "ambient-key")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "must-not-leak")
	provider, err := NewClaudeCLI(ClaudeCLIConfig{APIKey: "explicit-key", Timeout: time.Second})
	if err != nil {
		t.Fatalf("NewClaudeCLI() error = %v", err)
	}

	textArgs := strings.Join(provider.arguments(false, ""), " ")
	if !strings.Contains(textArgs, "--bare") || !strings.Contains(textArgs, "--tools ") {
		t.Fatalf("text args = %q", textArgs)
	}
	imageArgs := strings.Join(provider.arguments(true, "input.png"), " ")
	if !strings.Contains(imageArgs, "--tools Read") || !strings.Contains(imageArgs, "Read(./input.png)") {
		t.Fatalf("image args = %q", imageArgs)
	}

	environment := strings.Join(provider.environment(t.TempDir()), "\n")
	if strings.Contains(environment, "CLAUDE_CODE_OAUTH_TOKEN") || strings.Contains(environment, "must-not-leak") {
		t.Fatalf("OAuth credential leaked into environment: %s", environment)
	}
	if !strings.Contains(environment, "ANTHROPIC_API_KEY=explicit-key") {
		t.Fatal("explicit API key missing")
	}
	if !strings.Contains(environment, "HOME=") {
		t.Fatal("isolated HOME missing")
	}
}

func TestNewClaudeCLIRequiresAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := NewClaudeCLI(ClaudeCLIConfig{}); err == nil {
		t.Fatal("NewClaudeCLI() accepted empty API key")
	}
}
