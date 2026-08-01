package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "123456:ABC_def-token"

func TestGetUpdatesSendsExplicitAllowedUpdates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", request.Method)
		}
		if request.URL.Path != "/bot"+testToken+"/getUpdates" {
			t.Errorf("path = %q", request.URL.Path)
		}
		var payload struct {
			Offset         int64        `json:"offset"`
			Timeout        int          `json:"timeout"`
			AllowedUpdates []UpdateType `json:"allowed_updates"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if payload.Offset != 101 || payload.Timeout != 20 {
			t.Errorf("payload = %+v", payload)
		}
		want := ProductAllowedUpdates()
		if fmt.Sprint(payload.AllowedUpdates) != fmt.Sprint(want) {
			t.Errorf("allowed_updates = %v, want %v", payload.AllowedUpdates, want)
		}
		writeJSON(writer, `{"ok":true,"result":[{"update_id":101,"message":{"message_id":7,"date":1,"chat":{"id":42,"type":"private"},"text":"hi"}}]}`)
	}))
	defer server.Close()

	client := mustTestClient(t, server)
	updates, err := client.GetUpdates(context.Background(), GetUpdatesParams{Offset: 101, TimeoutSeconds: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].Message == nil || updates[0].Message.Text != "hi" {
		t.Fatalf("updates = %+v", updates)
	}
}

func TestSendMessageEncodesCopyAndCallbackButtons(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			ChatID      int64                 `json:"chat_id"`
			Text        string                `json:"text"`
			ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		if payload.ChatID != 42 || payload.Text != "готово" {
			t.Errorf("payload = %+v", payload)
		}
		if payload.ReplyMarkup == nil || len(payload.ReplyMarkup.InlineKeyboard) != 1 {
			t.Errorf("reply markup = %+v", payload.ReplyMarkup)
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		row := payload.ReplyMarkup.InlineKeyboard[0]
		if row[0].CopyText == nil || row[0].CopyText.Text != "скопируй меня" {
			t.Errorf("copy button = %+v", row[0])
		}
		if row[1].CallbackData != "more:1" {
			t.Errorf("callback button = %+v", row[1])
		}
		writeJSON(writer, `{"ok":true,"result":{"message_id":9,"date":1,"chat":{"id":42,"type":"private"},"text":"готово"}}`)
	}))
	defer server.Close()

	client := mustTestClient(t, server)
	message, err := client.SendMessage(context.Background(), SendMessageParams{
		ChatID: 42,
		Text:   "готово",
		ReplyMarkup: &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{
			CopyButton("Копировать", "скопируй меня"),
			CallbackButton("Ещё", "more:1"),
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if message.MessageID != 9 {
		t.Fatalf("message = %+v", message)
	}
}

func TestRateLimitRetryHonorsRetryAfter(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusTooManyRequests)
			writeJSON(writer, `{"ok":false,"error_code":429,"description":"slow down","parameters":{"retry_after":3}}`)
			return
		}
		writeJSON(writer, `{"ok":true,"result":{"id":12,"is_bot":true,"first_name":"Witty"}}`)
	}))
	defer server.Close()

	client := mustTestClient(t, server)
	var delay time.Duration
	client.sleep = func(_ context.Context, got time.Duration) error {
		delay = got
		return nil
	}
	user, err := client.GetMe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || delay != 3*time.Second || user.ID != 12 {
		t.Fatalf("calls=%d delay=%s user=%+v", calls.Load(), delay, user)
	}
}

func TestErrorsNeverExposeToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusBadRequest)
		writeJSON(writer, `{"ok":false,"error_code":400,"description":"bad URL /bot`+testToken+`/getMe"}`)
	}))
	defer server.Close()

	client := mustTestClient(t, server, WithMaxRateLimitRetries(0))
	_, err := client.GetMe(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaked token: %v", err)
	}
	var apiError *APIError
	if !errors.As(err, &apiError) || strings.Contains(apiError.Description, testToken) {
		t.Fatalf("unsafe API error: %#v", err)
	}

	client, err = New(testToken,
		WithBaseURL("http://telegram.invalid"),
		WithHTTPClient(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("dial %s", request.URL.String())
		})}),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetMe(context.Background())
	if !errors.Is(err, ErrNetwork) || strings.Contains(err.Error(), testToken) {
		t.Fatalf("transport error = %v", err)
	}
}

func TestAPIResponseBodyLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, strings.Repeat("x", 65))
	}))
	defer server.Close()

	client := mustTestClient(t, server, WithMaxResponseBytes(64))
	_, err := client.GetMe(context.Background())
	var tooLarge *ResponseTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.Limit != 64 {
		t.Fatalf("error = %T %v", err, err)
	}
}

func TestSendPhotoMultipart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := request.ParseMultipartForm(2 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		if got := request.FormValue("chat_id"); got != "42" {
			t.Errorf("chat_id = %q", got)
		}
		if got := request.FormValue("caption"); got != "подпись" {
			t.Errorf("caption = %q", got)
		}
		var markup InlineKeyboardMarkup
		if err := json.Unmarshal([]byte(request.FormValue("reply_markup")), &markup); err != nil {
			t.Errorf("reply markup: %v", err)
		}
		file, header, err := request.FormFile("photo")
		if err != nil {
			t.Errorf("photo form file: %v", err)
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		if header.Filename != "secret.png" || string(data) != "PNGDATA" {
			t.Errorf("file = %q %q", header.Filename, data)
		}
		writeJSON(writer, `{"ok":true,"result":{"message_id":10,"date":1,"chat":{"id":42,"type":"private"}}}`)
	}))
	defer server.Close()

	client := mustTestClient(t, server)
	message, err := client.SendPhoto(context.Background(), SendPhotoParams{
		ChatID:  42,
		Photo:   FileUpload("../../secret.png", []byte("PNGDATA")),
		Caption: "подпись",
		ReplyMarkup: &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{
			CallbackButton("Ещё", "more"),
		}}},
	})
	if err != nil || message.MessageID != 10 {
		t.Fatalf("message=%+v err=%v", message, err)
	}
}

func TestDownloadFileEnforcesKnownAndStreamedLimits(t *testing.T) {
	for _, test := range []struct {
		name             string
		metadataSize     int64
		body             string
		wantErr          bool
		wantDownloadCall bool
	}{
		{name: "within limit", metadataSize: 3, body: "abc", wantDownloadCall: true},
		{name: "known too large", metadataSize: 5, body: "abcde", wantErr: true},
		{name: "streamed too large", body: "abcde", wantErr: true, wantDownloadCall: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var downloaded atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/bot" + testToken + "/getFile":
					writeJSON(writer, fmt.Sprintf(`{"ok":true,"result":{"file_id":"f1","file_unique_id":"u1","file_size":%d,"file_path":"photos/a.bin"}}`, test.metadataSize))
				case "/file/bot" + testToken + "/photos/a.bin":
					downloaded.Store(true)
					if test.name == "streamed too large" {
						writer.Header().Set("Transfer-Encoding", "chunked")
					}
					_, _ = io.WriteString(writer, test.body)
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()

			client := mustTestClient(t, server, WithMaxDownloadBytes(4))
			file, err := client.DownloadFile(context.Background(), "f1")
			if (err != nil) != test.wantErr {
				t.Fatalf("DownloadFile() file=%+v err=%v", file, err)
			}
			if downloaded.Load() != test.wantDownloadCall {
				t.Fatalf("download called = %v, want %v", downloaded.Load(), test.wantDownloadCall)
			}
			if test.wantErr {
				var tooLarge *FileTooLargeError
				if !errors.As(err, &tooLarge) {
					t.Fatalf("error = %T %v", err, err)
				}
			} else if string(file.Data) != test.body {
				t.Fatalf("data = %q", file.Data)
			}
		})
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	var targetCalled atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetCalled.Store(true)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer source.Close()

	httpClient := source.Client()
	client, err := New(testToken, WithBaseURL(source.URL), WithHTTPClient(httpClient))
	if err != nil {
		t.Fatal(err)
	}
	if httpClient.CheckRedirect != nil {
		t.Fatal("constructor mutated the caller-owned HTTP client")
	}
	_, err = client.GetMe(context.Background())
	var httpError *HTTPError
	if !errors.As(err, &httpError) || httpError.StatusCode != http.StatusFound {
		t.Fatalf("error = %T %v", err, err)
	}
	if targetCalled.Load() {
		t.Fatal("redirect target was called")
	}
}

func TestClientRejectsCredentialsInBaseURL(t *testing.T) {
	_, err := New(testToken, WithBaseURL("https://user:password@api.telegram.org"))
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("New() error = %v", err)
	}
}

func mustTestClient(t *testing.T, server *httptest.Server, options ...Option) *Client {
	t.Helper()
	options = append([]Option{WithBaseURL(server.URL), WithHTTPClient(server.Client())}, options...)
	client, err := New(testToken, options...)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeJSON(writer http.ResponseWriter, body string) {
	writer.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(writer, body)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
