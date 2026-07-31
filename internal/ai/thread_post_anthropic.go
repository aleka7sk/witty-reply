package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

// GenerateThreadPost uses the same authenticated, redirect-safe Anthropic
// transport as Witty Reply while keeping a completely separate system prompt,
// schema, decoder, and semantic validator for first-party Belcanto posts.
func (provider *AnthropicProvider) GenerateThreadPost(ctx context.Context, request ThreadPostRequest) (ThreadPostResult, error) {
	if provider == nil {
		return ThreadPostResult{}, fmt.Errorf("%w: nil Anthropic provider", ErrConfiguration)
	}
	normalized, err := normalizeThreadPostRequest(request)
	if err != nil {
		return ThreadPostResult{}, err
	}
	prompt := buildThreadPostPrompt(normalized)
	body, err := provider.threadPostRequestBody(prompt)
	if err != nil {
		return ThreadPostResult{}, fmt.Errorf("%w: encode Anthropic Threads request: %v", ErrInvalidRequest, err)
	}

	var (
		nextDelay        time.Duration
		transportRetries int
		repairUsed       bool
	)
	for {
		if err := ctx.Err(); err != nil {
			return ThreadPostResult{}, err
		}
		if nextDelay > 0 {
			if err := sleepContext(ctx, nextDelay); err != nil {
				return ThreadPostResult{}, err
			}
			nextDelay = 0
		}

		result, retryAfter, retryable, callErr := provider.doThreadPostRequest(ctx, body, normalized)
		if callErr == nil {
			return result, nil
		}
		var providerErr *ProviderError
		if errors.As(callErr, &providerErr) && strings.HasPrefix(providerErr.Code, "invalid_output_") && !repairUsed {
			repairUsed = true
			repairPrompt := prompt + "\n\nThe previous post was rejected by the service validator (" + providerErr.Code + "). Generate one entirely fresh post from the original editorial controls. Correct the rejected condition, obey the schema and factual restrictions exactly, use no digits, and return no extra text."
			body, err = provider.threadPostRequestBody(repairPrompt)
			if err != nil {
				return ThreadPostResult{}, fmt.Errorf("%w: encode Anthropic Threads repair request: %v", ErrInvalidRequest, err)
			}
			continue
		}
		if !retryable || transportRetries >= provider.maxRetries {
			return ThreadPostResult{}, callErr
		}
		transportRetries++
		nextDelay = retryAfter
		if nextDelay <= 0 {
			nextDelay = provider.backoff(transportRetries)
		}
	}
}

func (provider *AnthropicProvider) threadPostRequestBody(prompt string) ([]byte, error) {
	payload := anthropicRequest{
		Model:     provider.model,
		MaxTokens: provider.maxTokens,
		System:    threadPostSystemPrompt,
		Messages: []anthropicMessage{{
			Role:    "user",
			Content: []anthropicContent{{Type: "text", Text: prompt}},
		}},
		OutputConfig: anthropicOutputConfig{
			Effort: provider.effort,
			Format: anthropicFormat{Type: "json_schema", Schema: threadPostJSONSchema()},
		},
	}
	return json.Marshal(payload)
}

func (provider *AnthropicProvider) doThreadPostRequest(ctx context.Context, body []byte, request normalizedThreadPostRequest) (ThreadPostResult, time.Duration, bool, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, provider.timeout)
	defer cancel()

	httpRequest, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, provider.endpoint, bytes.NewReader(body))
	if err != nil {
		return ThreadPostResult{}, 0, false, fmt.Errorf("%w: build Threads request: %v", ErrConfiguration, err)
	}
	httpRequest.Header.Set("content-type", "application/json")
	httpRequest.Header.Set("accept", "application/json")
	httpRequest.Header.Set("x-api-key", provider.apiKey)
	httpRequest.Header.Set("anthropic-version", anthropicVersion)
	httpRequest.Header.Set("user-agent", "witty-reply/1")

	response, err := provider.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return ThreadPostResult{}, 0, false, ctx.Err()
		}
		retryable := isRetryableNetworkError(err)
		providerErr := &ProviderError{Provider: providerAnthropic, Code: "transport_error", Retryable: retryable, Err: err}
		return ThreadPostResult{}, 0, retryable, providerErr
	}
	defer response.Body.Close()

	raw, readErr := readLimited(response.Body, defaultMaxResultBytes)
	if readErr != nil {
		retryable := response.StatusCode >= 500 || (response.StatusCode >= 200 && response.StatusCode < 300 && isRetryableNetworkError(readErr))
		providerErr := &ProviderError{
			Provider: providerAnthropic, Code: "read_error", Status: response.StatusCode,
			RequestID: response.Header.Get("request-id"), Retryable: retryable, Err: readErr,
		}
		return ThreadPostResult{}, 0, retryable, providerErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, retryAfter, retryable, apiErr := provider.apiError(response, raw)
		return ThreadPostResult{}, retryAfter, retryable, apiErr
	}

	var envelope anthropicResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ThreadPostResult{}, 0, false, &ProviderError{
			Provider: providerAnthropic, Code: "invalid_json", Status: response.StatusCode,
			RequestID: response.Header.Get("request-id"), Err: fmt.Errorf("%w: decode API envelope", ErrInvalidResponse),
		}
	}
	switch envelope.StopReason {
	case "end_turn", "stop_sequence":
	case "refusal":
		return ThreadPostResult{}, 0, false, &ProviderError{Provider: providerAnthropic, Code: "refusal", RequestID: response.Header.Get("request-id"), Err: ErrRefused}
	case "max_tokens", "model_context_window_exceeded":
		return ThreadPostResult{}, 0, false, &ProviderError{Provider: providerAnthropic, Code: envelope.StopReason, RequestID: response.Header.Get("request-id"), Err: ErrTruncated}
	default:
		return ThreadPostResult{}, 0, false, &ProviderError{
			Provider: providerAnthropic, Code: "unexpected_stop", RequestID: response.Header.Get("request-id"),
			Err: fmt.Errorf("%w: unexpected stop reason %q", ErrInvalidResponse, envelope.StopReason),
		}
	}

	var structured bytes.Buffer
	for _, block := range envelope.Content {
		if block.Type == "refusal" {
			return ThreadPostResult{}, 0, false, &ProviderError{Provider: providerAnthropic, Code: "refusal", RequestID: response.Header.Get("request-id"), Err: ErrRefused}
		}
		if block.Type == "text" {
			structured.WriteString(block.Text)
		}
	}
	result, err := decodeThreadPostResult(structured.Bytes(), request)
	if err != nil {
		return ThreadPostResult{}, 0, false, newInvalidOutputError(providerAnthropic, response.Header.Get("request-id"), err)
	}
	if envelope.Usage.InputTokens < 0 || envelope.Usage.OutputTokens < 0 || envelope.Usage.CacheCreationInputTokens < 0 || envelope.Usage.CacheReadInputTokens < 0 {
		return ThreadPostResult{}, 0, false, &ProviderError{Provider: providerAnthropic, Code: "invalid_usage", RequestID: response.Header.Get("request-id"), Err: ErrInvalidResponse}
	}
	result.Provider = providerAnthropic
	result.Model = envelope.Model
	if result.Model == "" {
		result.Model = provider.model
	}
	result.Usage = domain.Usage{
		InputTokens:  envelope.Usage.InputTokens + envelope.Usage.CacheCreationInputTokens + envelope.Usage.CacheReadInputTokens,
		OutputTokens: envelope.Usage.OutputTokens,
	}
	return result, 0, false, nil
}
