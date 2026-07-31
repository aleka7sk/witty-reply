package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleka7sk/witty-reply/internal/observability"
	"github.com/aleka7sk/witty-reply/internal/store"
)

func TestRunContextPollingSmoke(t *testing.T) {
	updatesObserved := make(chan struct{})
	var observedOnce sync.Once
	telegramAPI := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/getMe"):
			_, _ = fmt.Fprint(writer, `{"ok":true,"result":{"id":999,"is_bot":true,"first_name":"Witty","username":"witty_test_bot"}}`)
		case strings.HasSuffix(request.URL.Path, "/deleteWebhook"):
			_, _ = fmt.Fprint(writer, `{"ok":true,"result":true}`)
		case strings.HasSuffix(request.URL.Path, "/getUpdates"):
			observedOnce.Do(func() { close(updatesObserved) })
			time.Sleep(10 * time.Millisecond)
			_, _ = fmt.Fprint(writer, `{"ok":true,"result":[]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer telegramAPI.Close()

	t.Setenv("APP_ENV", "development")
	t.Setenv("LOG_LEVEL", "error")
	t.Setenv("TELEGRAM_BOT_TOKEN", "123456:test-token")
	t.Setenv("TELEGRAM_API_BASE_URL", telegramAPI.URL)
	t.Setenv("TELEGRAM_MODE", "polling")
	t.Setenv("TELEGRAM_CALLBACK_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("TELEGRAM_POLL_TIMEOUT", "1s")
	t.Setenv("TELEGRAM_REQUEST_TIMEOUT", "2s")
	t.Setenv("STORE_DRIVER", "memory")
	t.Setenv("AI_PROVIDER", "fake")
	t.Setenv("SPEECH_PROVIDER", "disabled")
	t.Setenv("HTTP_ADDRESS", "127.0.0.1:0")
	t.Setenv("SHUTDOWN_GRACE", "2s")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runContext(ctx) }()
	select {
	case <-updatesObserved:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("polling did not start")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runContext() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runContext did not shut down")
	}
}

func TestCleanupTimeoutDoesNotStopLaterSweeps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dataStore := &blockingCleanupStore{called: make(chan int, 4)}
	done := make(chan struct{})
	go func() {
		runCleanupSchedule(
			ctx, dataStore, time.Hour, 5*time.Millisecond, 10*time.Millisecond,
			observability.NewMetrics(), slog.New(slog.NewTextHandler(&strings.Builder{}, nil)),
		)
		close(done)
	}()

	select {
	case call := <-dataStore.called:
		if call != 1 {
			t.Fatalf("first cleanup call = %d", call)
		}
	case <-time.After(time.Second):
		t.Fatal("first cleanup did not start")
	}
	select {
	case call := <-dataStore.called:
		if call < 2 {
			t.Fatalf("later cleanup call = %d", call)
		}
		cancel()
	case <-time.After(time.Second):
		t.Fatal("timed-out cleanup prevented the next sweep")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup schedule did not stop")
	}
}

type blockingCleanupStore struct {
	store.Store
	calls  atomic.Int64
	called chan int
}

func (s *blockingCleanupStore) Cleanup(ctx context.Context, _ time.Time) (int64, error) {
	call := int(s.calls.Add(1))
	s.called <- call
	if call == 1 {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return 0, nil
}
