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
	providerCurated       = "curated_fallback"
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

// ThreadPostGenerator is deliberately separate from Provider. A Threads post
// is first-party Belcanto content, not a reply to a person or a comment under
// somebody else's publication, so it must never inherit Witty Reply's social
// mode classification or prompt contract.
type ThreadPostGenerator interface {
	GenerateThreadPost(context.Context, ThreadPostRequest) (ThreadPostResult, error)
}

var (
	_ Provider = (*AnthropicProvider)(nil)
	_ Provider = (*ClaudeCLIProvider)(nil)
	_ Provider = (*FakeProvider)(nil)

	_ ThreadPostGenerator = (*AnthropicProvider)(nil)
	_ ThreadPostGenerator = (*FakeProvider)(nil)
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

func invalidOutputCode(err error) string {
	message := strings.ToLower(fmt.Sprint(err))
	switch {
	case strings.Contains(message, "threads text contains forbidden current-anecdote narrative"):
		return "invalid_output_thread_current_anecdote"
	case strings.Contains(message, "threads text contains forbidden organization-experience narrative"):
		return "invalid_output_thread_organization_experience"
	case strings.Contains(message, "threads text contains forbidden first-person-experience narrative"):
		return "invalid_output_thread_first_person_experience"
	case strings.Contains(message, "threads text contains forbidden unverified-fact claim"):
		return "invalid_output_thread_unverified_fact"
	case strings.Contains(message, "threads text contains forbidden sales-pressure claim"):
		return "invalid_output_thread_sales_pressure"
	case strings.Contains(message, "threads text contains forbidden commercial claim"):
		return "invalid_output_thread_commercial_claim"
	case strings.Contains(message, "threads text contains digits"):
		return "invalid_output_thread_digits"
	case strings.Contains(message, "threads text does not match requested language"):
		return "invalid_output_thread_language"
	case strings.Contains(message, "threads text fails delivery moderation"):
		return "invalid_output_thread_delivery_safety"
	case strings.Contains(message, "repeated previous threads post"):
		return "invalid_output_thread_repeated_post"
	case strings.Contains(message, "repeated threads finalist"):
		return "invalid_output_thread_repeated_finalist"
	case strings.Contains(message, "unsupported threads goal"):
		return "invalid_output_thread_goal"
	case strings.Contains(message, "editorial quality gate: too long"):
		return "invalid_output_thread_quality_too_long"
	case strings.Contains(message, "editorial quality gate: ai_cliche"):
		return "invalid_output_thread_quality_cliche"
	case strings.Contains(message, "editorial quality gate: generic_question"):
		return "invalid_output_thread_quality_generic_question"
	case strings.Contains(message, "editorial quality gate"):
		return "invalid_output_thread_quality"
	case strings.Contains(message, "threads text") && strings.Contains(message, "exceeds"):
		return "invalid_output_thread_text_too_long"
	case strings.Contains(message, "threads text") && strings.Contains(message, "is empty"):
		return "invalid_output_thread_text_empty"
	case strings.Contains(message, "threads text") && strings.Contains(message, "invalid utf-8"):
		return "invalid_output_thread_text_encoding"
	case strings.Contains(message, "threads text"):
		return "invalid_output_thread_text"
	case strings.Contains(message, "decode threads structured output"), strings.Contains(message, "trailing threads"):
		return "invalid_output_thread_json"
	case strings.Contains(message, "threads reviewer"):
		return "invalid_output_thread_review"
	case strings.Contains(message, "expected exactly 3 replies"):
		return "invalid_output_reply_count"
	case strings.Contains(message, "duplicate reply"), strings.Contains(message, "repeated previous reply"):
		return "invalid_output_duplicate_reply"
	case strings.Contains(message, "exceeds"):
		return "invalid_output_text_too_long"
	case strings.Contains(message, "mode"), strings.Contains(message, "scenario"):
		return "invalid_output_mode"
	case strings.Contains(message, "tone"):
		return "invalid_output_tone"
	case strings.Contains(message, "decode structured output"), strings.Contains(message, "trailing"):
		return "invalid_output_json"
	default:
		return "invalid_output_semantic"
	}
}

func newInvalidOutputError(provider, requestID string, err error) *ProviderError {
	return &ProviderError{Provider: provider, Code: invalidOutputCode(err), RequestID: requestID, Err: err}
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
func decodeGenerationResult(raw []byte, requestedTone domain.Tone, requestedModes ...domain.ScenarioMode) (domain.GenerationResult, error) {
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
	if strings.TrimSpace(string(result.Mode)) == "" || strings.TrimSpace(string(result.ModeConfidence)) == "" {
		return domain.GenerationResult{}, fmt.Errorf("%w: structured output omitted mode classification", ErrInvalidResponse)
	}

	requestedMode := domain.ScenarioAuto
	if len(requestedModes) > 0 {
		requestedMode = requestedModes[0]
	}
	if err := ValidateResultForMode(&result, requestedTone, requestedMode); err != nil {
		return domain.GenerationResult{}, err
	}
	return result, nil
}
