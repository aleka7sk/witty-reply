package threads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMetaTextReplyFlowUsesExactEndpointsAndOrder(t *testing.T) {
	const (
		userID      = "user-123"
		accessToken = "test-access-token"
		postText    = "Не поздно начинать петь 🎼"
		replyToID   = "thread-456"
	)
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls = append(calls, request.Method+" "+request.URL.Path)
		switch len(calls) {
		case 1:
			if request.Method != http.MethodPost || request.URL.Path != "/v1.0/user-123/threads" {
				t.Errorf("unexpected create request: %s %s", request.Method, request.URL.Path)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			assertForm(t, request, map[string]string{
				"access_token": accessToken,
				"media_type":   "TEXT",
				"reply_to_id":  replyToID,
				"text":         postText,
			})
			writeJSON(writer, http.StatusOK, `{"id":"container-1"}`)
		case 2:
			if request.Method != http.MethodGet || request.URL.Path != "/v1.0/container-1" {
				t.Errorf("unexpected status request: %s %s", request.Method, request.URL.Path)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if got := request.URL.Query().Get("access_token"); got != accessToken {
				t.Errorf("unexpected status access token")
			}
			if got := request.URL.Query().Get("fields"); got != "id,status,error_message" {
				t.Errorf("fields = %q", got)
			}
			writeJSON(writer, http.StatusOK, `{"id":"container-1","status":"FINISHED"}`)
		case 3:
			if request.Method != http.MethodPost || request.URL.Path != "/v1.0/user-123/threads_publish" {
				t.Errorf("unexpected publish request: %s %s", request.Method, request.URL.Path)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			assertForm(t, request, map[string]string{
				"access_token": accessToken,
				"creation_id":  "container-1",
			})
			writeJSON(writer, http.StatusOK, `{"id":"publication-1"}`)
		default:
			t.Errorf("unexpected extra API request")
			writer.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client := newTestMeta(t, Config{
		UserID:      userID,
		AccessToken: accessToken,
		BaseURL:     server.URL + "/v1.0/",
	})
	containerID, err := client.CreateText(context.Background(), postText, replyToID)
	if err != nil {
		t.Fatalf("CreateText() error = %v", err)
	}
	if containerID != "container-1" {
		t.Fatalf("CreateText() id = %q", containerID)
	}
	status, err := client.ContainerStatus(context.Background(), containerID)
	if err != nil {
		t.Fatalf("ContainerStatus() error = %v", err)
	}
	if status.ID != containerID || status.State != StateFinished || !status.Ready() {
		t.Fatalf("ContainerStatus() = %+v", status)
	}
	publication, err := client.Publish(context.Background(), containerID)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if publication.ID != "publication-1" || publication.Permalink != "" {
		t.Fatalf("Publish() = %+v", publication)
	}

	wantCalls := []string{
		"POST /v1.0/user-123/threads",
		"GET /v1.0/container-1",
		"POST /v1.0/user-123/threads_publish",
	}
	if fmt.Sprint(calls) != fmt.Sprint(wantCalls) {
		t.Fatalf("calls = %v, want %v", calls, wantCalls)
	}
}

func TestMetaCreateFailureStopsBeforePublish(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(writer, http.StatusBadRequest, `{"error":{"message":"bad request","code":100}}`)
	}))
	defer server.Close()
	client := newTestMeta(t, Config{UserID: "user", AccessToken: "token", BaseURL: server.URL})

	containerID, err := client.CreateText(context.Background(), "text", "")
	if err == nil {
		_, err = client.Publish(context.Background(), containerID)
	}
	if err == nil {
		t.Fatal("flow unexpectedly succeeded")
	}
	if IsAmbiguous(err) {
		t.Fatalf("create rejection must be definite: %v", err)
	}
	if got := SafeCode(err); got != string(CodeInvalidInput) {
		t.Fatalf("SafeCode() = %q", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("request count = %d, want 1", got)
	}
}

func TestMetaPublishFailureClassification(t *testing.T) {
	t.Run("server error is ambiguous", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writeJSON(writer, http.StatusServiceUnavailable, `{"error":{"message":"temporary","code":2}}`)
		}))
		defer server.Close()
		client := newTestMeta(t, Config{UserID: "user", AccessToken: "token", BaseURL: server.URL})
		_, err := client.Publish(context.Background(), "container")
		if err == nil || !IsAmbiguous(err) {
			t.Fatalf("Publish() error = %v, want ambiguous", err)
		}
		if got := SafeCode(err); got != string(CodeUpstream) {
			t.Fatalf("SafeCode() = %q", got)
		}
	})

	t.Run("transport timeout is ambiguous", func(t *testing.T) {
		transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, context.DeadlineExceeded
		})
		client := newTestMeta(t, Config{
			UserID:      "user",
			AccessToken: "token",
			BaseURL:     "https://graph.threads.net/v1.0",
			HTTPClient:  &http.Client{Transport: transport},
		})
		_, err := client.Publish(context.Background(), "container")
		if err == nil || !IsAmbiguous(err) {
			t.Fatalf("Publish() error = %v, want ambiguous", err)
		}
		if got := SafeCode(err); got != string(CodeTransport) {
			t.Fatalf("SafeCode() = %q", got)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Publish() error does not preserve safe timeout sentinel: %v", err)
		}
	})
}

func TestMetaErrorsNeverExposeTokenTextOrUpstreamBody(t *testing.T) {
	const (
		token = "TOP_SECRET_ACCESS_TOKEN"
		text  = "private draft that must stay private"
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusBadRequest, `{"error":{"message":"`+token+` `+text+`","code":190,"fbtrace_id":"`+token+`"}}`)
	}))
	defer server.Close()
	client := newTestMeta(t, Config{UserID: "user", AccessToken: token, BaseURL: server.URL})

	_, err := client.CreateText(context.Background(), text, "")
	if err == nil {
		t.Fatal("CreateText() unexpectedly succeeded")
	}
	formatted := fmt.Sprintf("%v %+v", err, err)
	for _, secret := range []string{token, text, "private draft"} {
		if strings.Contains(formatted, secret) {
			t.Fatalf("error exposed sensitive request data: %q", formatted)
		}
	}
	if got := SafeCode(err); got != string(CodeAuth) {
		t.Fatalf("SafeCode() = %q", got)
	}
}

func TestMetaRefusesRedirectsEvenWithPermissiveHTTPClient(t *testing.T) {
	const token = "redirect-secret-token"
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		targetCalls.Add(1)
		if strings.Contains(request.URL.String(), token) {
			t.Error("redirect target received token")
		}
		writeJSON(writer, http.StatusOK, `{"id":"should-not-be-seen"}`)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/captured", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	permissive := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	client := newTestMeta(t, Config{
		UserID:      "user",
		AccessToken: token,
		BaseURL:     redirector.URL,
		HTTPClient:  permissive,
	})
	_, err := client.Publish(context.Background(), "container")
	if err == nil {
		t.Fatal("Publish() unexpectedly followed redirect")
	}
	if IsAmbiguous(err) || SafeCode(err) != string(CodeRedirect) {
		t.Fatalf("Publish() redirect error = %v", err)
	}
	if got := targetCalls.Load(); got != 0 {
		t.Fatalf("redirect target calls = %d", got)
	}
}

func TestMetaBoundsResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, `{"id":"`+strings.Repeat("x", 256)+`"}`)
	}))
	defer server.Close()
	client := newTestMeta(t, Config{
		UserID:           "user",
		AccessToken:      "token",
		BaseURL:          server.URL,
		MaxResponseBytes: 64,
	})

	_, err := client.Publish(context.Background(), "container")
	if err == nil || SafeCode(err) != string(CodeResponseTooLarge) {
		t.Fatalf("Publish() error = %v", err)
	}
	if !IsAmbiguous(err) {
		t.Fatalf("successful but unreadable publish response must be ambiguous: %v", err)
	}
}

func TestMetaValidatesUnicodeRuneLimitBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(writer, http.StatusOK, `{"id":"container"}`)
	}))
	defer server.Close()
	client := newTestMeta(t, Config{UserID: "user", AccessToken: "token", BaseURL: server.URL})

	if _, err := client.CreateText(context.Background(), strings.Repeat("я", MaxTextRunes), ""); err != nil {
		t.Fatalf("CreateText(500 runes) error = %v", err)
	}
	_, err := client.CreateText(context.Background(), strings.Repeat("я", MaxTextRunes+1), "")
	if err == nil || SafeCode(err) != string(CodeInvalidInput) || IsAmbiguous(err) {
		t.Fatalf("CreateText(501 runes) error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("request count = %d, want 1", got)
	}
}

func TestMetaConfigurationAndAuthenticationCodes(t *testing.T) {
	if _, err := NewMeta(Config{UserID: "user"}); err == nil || SafeCode(err) != string(CodeConfig) {
		t.Fatalf("missing-token error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusBadRequest, `{"error":{"code":190,"error_subcode":460,"fbtrace_id":"safe-trace"}}`)
	}))
	defer server.Close()
	client := newTestMeta(t, Config{UserID: "user", AccessToken: "token", BaseURL: server.URL})
	_, err := client.Publish(context.Background(), "container")
	if err == nil || SafeCode(err) != string(CodeAuth) || IsAmbiguous(err) {
		t.Fatalf("authentication error = %v", err)
	}
	var threadsErr *Error
	if !errors.As(err, &threadsErr) || threadsErr.GraphCode != 190 || threadsErr.GraphSubcode != 460 {
		t.Fatalf("authentication diagnostics = %+v", threadsErr)
	}
}

func TestDisabledAndFakePublishers(t *testing.T) {
	disabled := NewDisabled()
	if disabled.Enabled() {
		t.Fatal("disabled publisher reports enabled")
	}
	if _, err := disabled.CreateText(context.Background(), "text", ""); err == nil || SafeCode(err) != string(CodeDisabled) || !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled CreateText() error = %v", err)
	}

	fake := NewFake()
	if !fake.Enabled() {
		t.Fatal("fake publisher reports disabled")
	}
	first, err := fake.CreateText(context.Background(), "first", "")
	if err != nil || first != "fake-container-000001" {
		t.Fatalf("first fake container = %q, %v", first, err)
	}
	second, err := fake.CreateText(context.Background(), "second", "thread-1")
	if err != nil || second != "fake-container-000002" {
		t.Fatalf("second fake container = %q, %v", second, err)
	}
	status, err := fake.ContainerStatus(context.Background(), first)
	if err != nil || status.State != StateFinished {
		t.Fatalf("fake status before publish = %+v, %v", status, err)
	}
	publication, err := fake.Publish(context.Background(), first)
	if err != nil || publication.ID != "fake-publication-000001" {
		t.Fatalf("fake publication = %+v, %v", publication, err)
	}
	again, err := fake.Publish(context.Background(), first)
	if err != nil || again != publication {
		t.Fatalf("repeated fake publication = %+v, %v", again, err)
	}
	status, err = fake.ContainerStatus(context.Background(), first)
	if err != nil || status.State != StatePublished {
		t.Fatalf("fake status after publish = %+v, %v", status, err)
	}
}

func newTestMeta(t *testing.T, config Config) *Meta {
	t.Helper()
	client, err := NewMeta(config)
	if err != nil {
		t.Fatalf("NewMeta() error = %v", err)
	}
	return client
}

func assertForm(t *testing.T, request *http.Request, want map[string]string) {
	t.Helper()
	if got := request.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q", got)
	}
	if request.URL.RawQuery != "" {
		t.Errorf("POST query must be empty")
	}
	if err := request.ParseForm(); err != nil {
		t.Errorf("ParseForm() error = %v", err)
		return
	}
	if len(request.Form) != len(want) {
		t.Errorf("form field count = %d, want %d", len(request.Form), len(want))
	}
	for name, value := range want {
		if got := request.Form.Get(name); got != value {
			t.Errorf("form field %s does not match", name)
		}
	}
}

func writeJSON(writer http.ResponseWriter, status int, body string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, body)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
