package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

const anthropicVersion = "2023-06-01"

// AnthropicConfig controls the direct Messages API provider. Temperature is
// deliberately absent: structured output plus low effort is the stable product
// contract, and the current API does not need sampling controls for this task.
type AnthropicConfig struct {
	APIKey     string
	BaseURL    string
	Model      string
	Effort     string
	MaxTokens  int
	Timeout    time.Duration
	MaxRetries int
	RetryBase  time.Duration
	HTTPClient *http.Client
}

type AnthropicProvider struct {
	apiKey     string
	endpoint   string
	model      string
	effort     string
	maxTokens  int
	timeout    time.Duration
	maxRetries int
	retryBase  time.Duration
	client     *http.Client
}

func NewAnthropic(config AnthropicConfig) (*AnthropicProvider, error) {
	config.APIKey = strings.TrimSpace(config.APIKey)
	if config.APIKey == "" {
		return nil, fmt.Errorf("%w: Anthropic API key is required", ErrConfiguration)
	}
	config.BaseURL = strings.TrimSpace(config.BaseURL)
	if config.BaseURL == "" {
		config.BaseURL = "https://api.anthropic.com"
	}
	baseURL, err := validateBaseURL(config.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("%w: Anthropic base URL: %v", ErrConfiguration, err)
	}
	config.Model = strings.TrimSpace(config.Model)
	if config.Model == "" {
		config.Model = defaultModel
	}
	if !validModelName(config.Model) {
		return nil, fmt.Errorf("%w: invalid Anthropic model name", ErrConfiguration)
	}
	config.Effort = strings.ToLower(strings.TrimSpace(config.Effort))
	if config.Effort == "" {
		config.Effort = "low"
	}
	switch config.Effort {
	case "low", "medium", "high":
	default:
		return nil, fmt.Errorf("%w: effort must be low, medium, or high", ErrConfiguration)
	}
	if config.MaxTokens == 0 {
		config.MaxTokens = defaultMaxTokens
	}
	if config.MaxTokens < 256 || config.MaxTokens > 64_000 {
		return nil, fmt.Errorf("%w: max tokens must be between 256 and 64000", ErrConfiguration)
	}
	if config.Timeout == 0 {
		config.Timeout = 45 * time.Second
	}
	if config.Timeout < time.Second {
		return nil, fmt.Errorf("%w: timeout must be at least one second", ErrConfiguration)
	}
	if config.MaxRetries < 0 || config.MaxRetries > 5 {
		return nil, fmt.Errorf("%w: max retries must be between 0 and 5", ErrConfiguration)
	}
	if config.RetryBase == 0 {
		config.RetryBase = 250 * time.Millisecond
	}
	if config.RetryBase < 0 || config.RetryBase > 10*time.Second {
		return nil, fmt.Errorf("%w: invalid retry base", ErrConfiguration)
	}
	client := &http.Client{}
	if config.HTTPClient != nil {
		clientCopy := *config.HTTPClient
		client = &clientCopy
	}
	if client.CheckRedirect == nil {
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	return &AnthropicProvider{
		apiKey:     config.APIKey,
		endpoint:   strings.TrimRight(baseURL, "/") + "/v1/messages",
		model:      config.Model,
		effort:     config.Effort,
		maxTokens:  config.MaxTokens,
		timeout:    config.Timeout,
		maxRetries: config.MaxRetries,
		retryBase:  config.RetryBase,
		client:     client,
	}, nil
}

// NewAnthropicProvider is an explicit-name alias useful at composition sites.
func NewAnthropicProvider(config AnthropicConfig) (*AnthropicProvider, error) {
	return NewAnthropic(config)
}

func (provider *AnthropicProvider) Generate(ctx context.Context, request domain.GenerationRequest) (domain.GenerationResult, error) {
	if provider == nil {
		return domain.GenerationResult{}, fmt.Errorf("%w: nil Anthropic provider", ErrConfiguration)
	}
	normalized, prompt, err := prepareRequest(request, "")
	if err != nil {
		return domain.GenerationResult{}, err
	}
	body, err := provider.requestBody(normalized, prompt)
	if err != nil {
		return domain.GenerationResult{}, fmt.Errorf("%w: encode Anthropic request: %v", ErrInvalidRequest, err)
	}

	var (
		lastErr   error
		nextDelay time.Duration
	)
	for attempt := 0; attempt <= provider.maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return domain.GenerationResult{}, err
		}
		if attempt > 0 {
			if err := sleepContext(ctx, nextDelay); err != nil {
				return domain.GenerationResult{}, err
			}
		}

		result, retryAfter, retryable, callErr := provider.doRequest(ctx, body, normalized.Tone)
		if callErr == nil {
			return result, nil
		}
		lastErr = callErr
		if !retryable || attempt == provider.maxRetries {
			return domain.GenerationResult{}, callErr
		}
		nextDelay = retryAfter
		if nextDelay <= 0 {
			nextDelay = provider.backoff(attempt + 1)
		}
	}
	return domain.GenerationResult{}, lastErr
}

func (provider *AnthropicProvider) requestBody(request domain.GenerationRequest, prompt string) ([]byte, error) {
	content := make([]anthropicContent, 0, 2)
	if request.Input.Kind == domain.InputImage {
		content = append(content, anthropicContent{
			Type: "image",
			Source: &anthropicImageSource{
				Type:      "base64",
				MediaType: request.Input.MediaType,
				Data:      base64.StdEncoding.EncodeToString(request.Input.Image),
			},
		})
	}
	content = append(content, anthropicContent{Type: "text", Text: prompt})

	payload := anthropicRequest{
		Model:     provider.model,
		MaxTokens: provider.maxTokens,
		System:    systemPrompt,
		Messages: []anthropicMessage{{
			Role:    "user",
			Content: content,
		}},
		OutputConfig: anthropicOutputConfig{
			Effort: provider.effort,
			Format: anthropicFormat{
				Type:   "json_schema",
				Schema: GenerationJSONSchema(),
			},
		},
	}
	return json.Marshal(payload)
}

func (provider *AnthropicProvider) doRequest(ctx context.Context, body []byte, requestedTone domain.Tone) (domain.GenerationResult, time.Duration, bool, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, provider.timeout)
	defer cancel()

	httpRequest, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, provider.endpoint, bytes.NewReader(body))
	if err != nil {
		return domain.GenerationResult{}, 0, false, fmt.Errorf("%w: build request: %v", ErrConfiguration, err)
	}
	httpRequest.Header.Set("content-type", "application/json")
	httpRequest.Header.Set("accept", "application/json")
	httpRequest.Header.Set("x-api-key", provider.apiKey)
	httpRequest.Header.Set("anthropic-version", anthropicVersion)
	httpRequest.Header.Set("user-agent", "witty-reply/1")

	response, err := provider.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return domain.GenerationResult{}, 0, false, ctx.Err()
		}
		retryable := isRetryableNetworkError(err)
		providerErr := &ProviderError{Provider: providerAnthropic, Code: "transport_error", Retryable: retryable, Err: err}
		return domain.GenerationResult{}, 0, retryable, providerErr
	}
	defer response.Body.Close()

	raw, readErr := readLimited(response.Body, defaultMaxResultBytes)
	if readErr != nil {
		retryable := response.StatusCode >= 500 || (response.StatusCode >= 200 && response.StatusCode < 300 && isRetryableNetworkError(readErr))
		providerErr := &ProviderError{
			Provider:  providerAnthropic,
			Code:      "read_error",
			Status:    response.StatusCode,
			RequestID: response.Header.Get("request-id"),
			Retryable: retryable,
			Err:       readErr,
		}
		return domain.GenerationResult{}, 0, providerErr.Retryable, providerErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.apiError(response, raw)
	}

	var envelope anthropicResponse
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return domain.GenerationResult{}, 0, false, &ProviderError{
			Provider:  providerAnthropic,
			Code:      "invalid_json",
			Status:    response.StatusCode,
			RequestID: response.Header.Get("request-id"),
			Err:       fmt.Errorf("%w: decode API envelope", ErrInvalidResponse),
		}
	}
	switch envelope.StopReason {
	case "end_turn", "stop_sequence":
	case "refusal":
		return domain.GenerationResult{}, 0, false, &ProviderError{Provider: providerAnthropic, Code: "refusal", RequestID: response.Header.Get("request-id"), Err: ErrRefused}
	case "max_tokens", "model_context_window_exceeded":
		return domain.GenerationResult{}, 0, false, &ProviderError{Provider: providerAnthropic, Code: envelope.StopReason, RequestID: response.Header.Get("request-id"), Err: ErrTruncated}
	default:
		return domain.GenerationResult{}, 0, false, &ProviderError{Provider: providerAnthropic, Code: "unexpected_stop", RequestID: response.Header.Get("request-id"), Err: fmt.Errorf("%w: unexpected stop reason %q", ErrInvalidResponse, envelope.StopReason)}
	}

	var structured bytes.Buffer
	for _, block := range envelope.Content {
		if block.Type == "refusal" {
			return domain.GenerationResult{}, 0, false, &ProviderError{Provider: providerAnthropic, Code: "refusal", RequestID: response.Header.Get("request-id"), Err: ErrRefused}
		}
		if block.Type == "text" {
			structured.WriteString(block.Text)
		}
	}
	result, err := decodeGenerationResult(structured.Bytes(), requestedTone)
	if err != nil {
		return domain.GenerationResult{}, 0, false, &ProviderError{Provider: providerAnthropic, Code: "invalid_output", RequestID: response.Header.Get("request-id"), Err: err}
	}
	result.Provider = providerAnthropic
	result.Model = envelope.Model
	if result.Model == "" {
		result.Model = provider.model
	}
	if envelope.Usage.InputTokens < 0 || envelope.Usage.OutputTokens < 0 || envelope.Usage.CacheCreationInputTokens < 0 || envelope.Usage.CacheReadInputTokens < 0 {
		return domain.GenerationResult{}, 0, false, &ProviderError{Provider: providerAnthropic, Code: "invalid_usage", RequestID: response.Header.Get("request-id"), Err: ErrInvalidResponse}
	}
	result.Usage = domain.Usage{
		InputTokens:  envelope.Usage.InputTokens + envelope.Usage.CacheCreationInputTokens + envelope.Usage.CacheReadInputTokens,
		OutputTokens: envelope.Usage.OutputTokens,
	}
	return result, 0, false, nil
}

func (provider *AnthropicProvider) apiError(response *http.Response, raw []byte) (domain.GenerationResult, time.Duration, bool, error) {
	var envelope anthropicErrorResponse
	_ = json.Unmarshal(raw, &envelope)
	code := strings.TrimSpace(envelope.Error.Type)
	if code == "" {
		code = "http_error"
	}
	retryable := retryableStatus(response.StatusCode)
	providerErr := &ProviderError{
		Provider:  providerAnthropic,
		Code:      code,
		Status:    response.StatusCode,
		RequestID: firstNonEmpty(response.Header.Get("request-id"), envelope.RequestID),
		Retryable: retryable,
		Err:       fmt.Errorf("anthropic API returned %s", response.Status),
	}
	return domain.GenerationResult{}, parseRetryAfter(response.Header.Get("retry-after")), retryable, providerErr
}

func (provider *AnthropicProvider) backoff(attempt int) time.Duration {
	delay := provider.retryBase
	for power := 1; power < attempt; power++ {
		delay *= 2
	}
	if delay > 10*time.Second {
		return 10 * time.Second
	}
	return delay
}

func validateBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "https" {
		host := parsed.Hostname()
		if parsed.Scheme != "http" || (host != "localhost" && net.ParseIP(host) == nil) {
			return "", errors.New("HTTPS is required (HTTP is allowed only for loopback testing)")
		}
		ip := net.ParseIP(host)
		if ip != nil && !ip.IsLoopback() {
			return "", errors.New("HTTP is allowed only for loopback testing")
		}
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("URL must contain a host and no credentials, query, or fragment")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusConflict || status == http.StatusTooManyRequests || status >= 500
}

func isRetryableNetworkError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0
		}
		delay := time.Duration(seconds) * time.Second
		if delay > 30*time.Second {
			return 30 * time.Second
		}
		return delay
	}
	if deadline, err := http.ParseTime(value); err == nil {
		delay := time.Until(deadline)
		if delay <= 0 {
			return 0
		}
		if delay > 30*time.Second {
			return 30 * time.Second
		}
		return delay
	}
	return 0
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func readLimited(reader io.Reader, maximum int64) ([]byte, error) {
	limited := io.LimitReader(reader, maximum+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maximum {
		return nil, ErrOutputTooLarge
	}
	return raw, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type anthropicRequest struct {
	Model        string                `json:"model"`
	MaxTokens    int                   `json:"max_tokens"`
	System       string                `json:"system"`
	Messages     []anthropicMessage    `json:"messages"`
	OutputConfig anthropicOutputConfig `json:"output_config"`
}

type anthropicMessage struct {
	Role    string             `json:"role"`
	Content []anthropicContent `json:"content"`
}

type anthropicContent struct {
	Type   string                `json:"type"`
	Text   string                `json:"text,omitempty"`
	Source *anthropicImageSource `json:"source,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicOutputConfig struct {
	Effort string          `json:"effort"`
	Format anthropicFormat `json:"format"`
}

type anthropicFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

type anthropicResponse struct {
	Model      string `json:"model"`
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Usage struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

type anthropicErrorResponse struct {
	RequestID string `json:"request_id"`
	Error     struct {
		Type string `json:"type"`
	} `json:"error"`
}
