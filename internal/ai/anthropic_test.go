package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

func TestAnthropicProviderRetriesAndSendsStructuredImageRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		call := calls.Add(1)
		if request.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.Header.Get("x-api-key") != "test-key" {
			t.Errorf("x-api-key = %q", request.Header.Get("x-api-key"))
		}
		if request.Header.Get("anthropic-version") != anthropicVersion {
			t.Errorf("anthropic-version = %q", request.Header.Get("anthropic-version"))
		}
		if call == 1 {
			writer.Header().Set("retry-after", "0")
			writer.WriteHeader(http.StatusTooManyRequests)
			_, _ = writer.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error"}}`))
			return
		}

		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if _, found := payload["temperature"]; found {
			t.Error("temperature must not be sent")
		}
		outputConfig, ok := payload["output_config"].(map[string]any)
		if !ok || outputConfig["effort"] != "low" {
			t.Errorf("output_config = %#v", payload["output_config"])
		}
		format, ok := outputConfig["format"].(map[string]any)
		if !ok {
			t.Errorf("output format = %#v", outputConfig["format"])
		} else {
			assertNoUnsupportedSchemaConstraints(t, format["schema"])
		}
		messages := payload["messages"].([]any)
		content := messages[0].(map[string]any)["content"].([]any)
		if content[0].(map[string]any)["type"] != "image" || content[1].(map[string]any)["type"] != "text" {
			t.Errorf("content order = %#v", content)
		}
		source := content[0].(map[string]any)["source"].(map[string]any)
		if source["media_type"] != "image/png" || source["data"] != base64.StdEncoding.EncodeToString(pngHeader()) {
			t.Errorf("image source = %#v", source)
		}

		structured, _ := json.Marshal(validMixedResult())
		response := map[string]any{
			"model":       "claude-sonnet-5-20260701",
			"stop_reason": "end_turn",
			"content":     []any{map[string]any{"type": "text", "text": string(structured)}},
			"usage":       map[string]any{"input_tokens": 31, "output_tokens": 19},
		}
		writer.Header().Set("content-type", "application/json")
		writer.Header().Set("request-id", "req_test")
		_ = json.NewEncoder(writer).Encode(response)
	}))
	defer server.Close()

	provider, err := NewAnthropic(AnthropicConfig{
		APIKey:     "test-key",
		BaseURL:    server.URL,
		Model:      defaultModel,
		Effort:     "low",
		Timeout:    time.Second,
		MaxRetries: 1,
		RetryBase:  time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewAnthropic() error = %v", err)
	}
	result, err := provider.Generate(context.Background(), domain.GenerationRequest{
		Input: domain.Input{Kind: domain.InputImage, Image: pngHeader(), MediaType: "image/png"},
		Tone:  domain.ToneMix,
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d", calls.Load())
	}
	if result.Provider != providerAnthropic || result.Model != "claude-sonnet-5-20260701" {
		t.Fatalf("result metadata = provider %q model %q", result.Provider, result.Model)
	}
	if result.Usage.InputTokens != 31 || result.Usage.OutputTokens != 19 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func assertNoUnsupportedSchemaConstraints(t *testing.T, value any) {
	t.Helper()
	unsupported := map[string]struct{}{
		"minimum": {}, "maximum": {}, "exclusiveMinimum": {}, "exclusiveMaximum": {},
		"minLength": {}, "maxLength": {}, "minItems": {}, "maxItems": {},
	}
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				if _, blocked := unsupported[key]; blocked {
					t.Errorf("provider schema contains unsupported constraint %q", key)
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
}

func TestAnthropicProviderMapsTruncation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"model": "claude-sonnet-5", "stop_reason": "max_tokens", "content": []any{},
		})
	}))
	defer server.Close()
	provider, err := NewAnthropic(AnthropicConfig{APIKey: "test", BaseURL: server.URL, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Generate(context.Background(), textRequest(domain.ToneMix))
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("truncation error = %v", err)
	}
}

func pngHeader() []byte {
	return []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
}
