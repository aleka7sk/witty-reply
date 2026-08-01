package transcribe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenAICompatibleTranscribe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/audio/transcriptions" || request.Method != http.MethodPost {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if request.FormValue("model") != "whisper-test" || request.FormValue("language") != "ru" {
			t.Errorf("unexpected form: %+v", request.Form)
		}
		file, header, err := request.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		if string(data) != "audio-data" || header.Filename != "voice.ogg" {
			t.Errorf("file = %q, %q", data, header.Filename)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"text":"  Привет, мир  ","language":"ru","duration":1.5,"segments":[{"avg_logprob":-0.1}]}`))
	}))
	defer server.Close()

	adapter, err := NewOpenAICompatible(OpenAIConfig{
		BaseURL: server.URL + "/v1", APIKey: "secret", Model: "whisper-test",
		Timeout: time.Second, ResponseFormat: "verbose_json",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Transcribe(context.Background(), Audio{
		Data: []byte("audio-data"), Filename: "../voice.ogg", MediaType: "audio/ogg", Language: "ru",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "Привет, мир" || result.Language != "ru" || result.Confidence <= 0 || result.Provider != "openai_compatible" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestOpenAICompatibleValidatesAudioBeforeRequest(t *testing.T) {
	adapter, err := NewOpenAICompatible(OpenAIConfig{BaseURL: "https://example.test/v1", Model: "m", MaxAudioBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Transcribe(context.Background(), Audio{}); !errors.Is(err, ErrEmptyAudio) {
		t.Fatalf("empty error = %v", err)
	}
	if _, err := adapter.Transcribe(context.Background(), Audio{Data: []byte("1234")}); !errors.Is(err, ErrAudioTooLarge) {
		t.Fatalf("size error = %v", err)
	}
}

func TestOpenAICompatibleReturnsTypedBoundedHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"error":{"type":"rate_limit","message":"slow down"}}`))
	}))
	defer server.Close()
	adapter, err := NewOpenAICompatible(OpenAIConfig{BaseURL: server.URL, Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Transcribe(context.Background(), Audio{Data: []byte("x")})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests || !strings.Contains(err.Error(), "rate_limit") {
		t.Fatalf("unexpected error: %T %v", err, err)
	}
}

func TestOpenAICompatibleRejectsUnsafeBaseURLs(t *testing.T) {
	tests := []string{
		"http://speech.example.com/v1",
		"https://user:password@speech.example.com/v1",
		"https://speech.example.com/v1?tenant=private",
		"https://speech.example.com/v1#fragment",
	}
	for _, baseURL := range tests {
		t.Run(baseURL, func(t *testing.T) {
			if _, err := NewOpenAICompatible(OpenAIConfig{BaseURL: baseURL, Model: "m"}); err == nil {
				t.Fatalf("NewOpenAICompatible(%q) unexpectedly succeeded", baseURL)
			}
		})
	}
}

func TestOpenAICompatibleDoesNotFollowRedirectsOrMutateClient(t *testing.T) {
	var targetCalled atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalled.Store(true)
	}))
	defer target.Close()

	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	httpClient := source.Client()
	if httpClient.CheckRedirect != nil {
		t.Fatal("test client unexpectedly has a redirect policy")
	}
	adapter, err := NewOpenAICompatible(OpenAIConfig{BaseURL: source.URL, Model: "m", HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	if httpClient.CheckRedirect != nil {
		t.Fatal("constructor mutated the caller-owned HTTP client")
	}
	_, err = adapter.Transcribe(context.Background(), Audio{Data: []byte("private voice")})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("Transcribe() error = %T %v", err, err)
	}
	if targetCalled.Load() {
		t.Fatal("redirect target received the private audio body")
	}
}

func TestDisabledTranscriber(t *testing.T) {
	var transcriber Transcriber = NewDisabled()
	if _, err := transcriber.Transcribe(context.Background(), Audio{Data: []byte("x")}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("error = %v", err)
	}
}
