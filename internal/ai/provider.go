// Package ai contains the model-provider boundary used by witty-reply.
//
// Providers receive already-authorized domain requests, construct a prompt that
// treats all user supplied material as untrusted data, and return a validated
// domain result.  Callers must still escape reply text for the Telegram parse
// mode they choose when rendering it.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

const (
	defaultModel          = "claude-sonnet-5"
	defaultMaxTokens      = 2048
	defaultMaxTextRunes   = 12_000
	defaultMaxImageBytes  = 10 << 20
	defaultMaxResultBytes = 2 << 20
	defaultMaxCLIOutput   = 4 << 20
	providerAnthropic     = "anthropic"
	providerClaudeCLI     = "claude_cli"
	providerFake          = "fake"
)

var (
	ErrInvalidRequest  = errors.New("invalid AI request")
	ErrInvalidResponse = errors.New("invalid AI response")
	ErrConfiguration   = errors.New("invalid AI provider configuration")
	ErrOutputTooLarge  = errors.New("AI provider output is too large")
	ErrRefused         = errors.New("AI provider refused the request")
	ErrTruncated       = errors.New("AI provider response was truncated")
)

// Provider is deliberately small so production, local-CLI and deterministic
// implementations can be interchanged without leaking transport details into
// the application layer.
type Provider interface {
	Generate(context.Context, domain.GenerationRequest) (domain.GenerationResult, error)
}

var (
	_ Provider = (*AnthropicProvider)(nil)
	_ Provider = (*ClaudeCLIProvider)(nil)
	_ Provider = (*FakeProvider)(nil)
)

// ProviderError is safe to inspect with errors.As.  It intentionally does not
// contain request or response bodies, which may include private conversations.
type ProviderError struct {
	Provider  string
	Code      string
	Status    int
	RequestID string
	Retryable bool
	Err       error
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "<nil>"
	}
	parts := []string{e.Provider + " request failed"}
	if e.Code != "" {
		parts = append(parts, "code="+e.Code)
	}
	if e.Status != 0 {
		parts = append(parts, fmt.Sprintf("status=%d", e.Status))
	}
	if e.RequestID != "" {
		parts = append(parts, "request_id="+e.RequestID)
	}
	if e.Err != nil {
		parts = append(parts, e.Err.Error())
	}
	return strings.Join(parts, ": ")
}

func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func validModelName(value string) bool {
	if value == "" || len(value) > 160 || value[0] == '-' {
		return false
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case strings.ContainsRune("-_.:/", character):
		default:
			return false
		}
	}
	return true
}

// decodeGenerationResult rejects extra fields and trailing JSON before the
// semantic validator is run. Structured model output is not trusted merely
// because the upstream API claims it matches a schema.
func decodeGenerationResult(raw []byte, requestedTone domain.Tone) (domain.GenerationResult, error) {
	if len(raw) == 0 {
		return domain.GenerationResult{}, fmt.Errorf("%w: empty structured output", ErrInvalidResponse)
	}
	if len(raw) > defaultMaxResultBytes {
		return domain.GenerationResult{}, ErrOutputTooLarge
	}

	var result domain.GenerationResult
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return domain.GenerationResult{}, fmt.Errorf("%w: decode structured output: %v", ErrInvalidResponse, err)
	}
	if err := decoder.Decode(&struct{}{}); err == nil {
		return domain.GenerationResult{}, fmt.Errorf("%w: trailing JSON value", ErrInvalidResponse)
	} else if !errors.Is(err, io.EOF) {
		return domain.GenerationResult{}, fmt.Errorf("%w: malformed trailing data: %v", ErrInvalidResponse, err)
	}

	if err := ValidateResult(&result, requestedTone); err != nil {
		return domain.GenerationResult{}, err
	}
	return result, nil
}
