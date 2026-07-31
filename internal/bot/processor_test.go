package bot

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aleka7sk/witty-reply/internal/ai"
	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/aleka7sk/witty-reply/internal/observability"
	"github.com/aleka7sk/witty-reply/internal/session"
	"github.com/aleka7sk/witty-reply/internal/store"
	"github.com/aleka7sk/witty-reply/internal/telegram"
)

const testQueueSecret = "0123456789abcdef0123456789abcdef"

func TestProcessorRejectsInvalidConfiguration(t *testing.T) {
	if _, err := NewProcessor(context.Background(), nil, store.NewMemory(), testQueueSecret, 1, 1, nil, nil); err == nil {
		t.Fatal("nil service was accepted")
	}
	service, _, _, _, _ := newTestService(t, ai.NewFake())
	if _, err := NewProcessor(context.Background(), service, nil, testQueueSecret, 1, 1, nil, nil); err == nil {
		t.Fatal("nil store was accepted")
	}
	if _, err := NewProcessor(context.Background(), service, store.NewMemory(), "short", 1, 1, nil, nil); err == nil {
		t.Fatal("short encryption secret was accepted")
	}
}

func TestProcessorDurablyDeliversAndDeduplicatesUpdate(t *testing.T) {
	provider := &countingProvider{next: ai.NewFake()}
	service, telegramClient, memory, sessions, codec := newTestService(t, provider)
	_, _ = memory.UpsertUser(context.Background(), domain.User{TelegramID: 42, Language: "ru"})
	_ = memory.SetConsent(context.Background(), 42, true)
	processor, err := NewProcessor(context.Background(), service, memory, testQueueSecret, 1, 2, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer processor.Close()
	update := textUpdate(1, "queued")
	if err := processor.Enqueue(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	if err := processor.Enqueue(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return provider.calls.Load() == 1 })
	time.Sleep(20 * time.Millisecond)
	if provider.calls.Load() != 1 {
		t.Fatalf("provider calls = %d", provider.calls.Load())
	}
	waitFor(t, time.Second, func() bool {
		pending, _, statsErr := memory.QueueStats(context.Background(), time.Now())
		return statsErr == nil && pending == 0
	})
	if snapshot, ok := sessions.Get(42); !ok || snapshot.Value.GenerationID == 0 {
		t.Fatalf("completed processor job lost interaction state: %+v, %v", snapshot, ok)
	} else {
		callbackData, encodeErr := codec.Encode(session.CallbackPayload{
			Action: session.ActionMore, UserID: 42, InteractionID: snapshot.Value.GenerationID,
			Revision: snapshot.Value.Revision, Candidate: -1,
		})
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		user := testUser()
		callbackMessage := telegram.Message{MessageID: 2, Chat: telegram.Chat{ID: 42, Type: "private"}}
		if err := processor.Enqueue(context.Background(), telegram.Update{
			UpdateID: 2,
			CallbackQuery: &telegram.CallbackQuery{
				ID: "refine-after-completion", From: user, Message: &callbackMessage, Data: callbackData,
			},
		}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, time.Second, func() bool { return provider.calls.Load() == 2 })
		waitFor(t, time.Second, func() bool { return len(telegramClient.snapshotMessages()) == 2 })
	}
}

func TestProcessorRetriesFailureBeforeCompleting(t *testing.T) {
	service, telegramClient, memory, _, _ := newTestService(t, ai.NewFake())
	flaky := &flakyTelegram{fakeTelegram: telegramClient}
	flaky.failures.Store(1)
	service.telegram = flaky
	processor, err := NewProcessor(context.Background(), service, memory, testQueueSecret, 1, 1, nil, nil,
		processorTestTimings(time.Millisecond, time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer processor.Close()
	if err := processor.Enqueue(context.Background(), textUpdate(9, "/help")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return len(telegramClient.snapshotMessages()) == 1 })
	if flaky.calls.Load() != 2 {
		t.Fatalf("send attempts = %d", flaky.calls.Load())
	}
}

func TestProcessorDeadLettersAfterEightAttempts(t *testing.T) {
	service, telegramClient, memory, _, _ := newTestService(t, ai.NewFake())
	flaky := &flakyTelegram{fakeTelegram: telegramClient}
	flaky.failures.Store(100)
	service.telegram = flaky
	processor, err := NewProcessor(context.Background(), service, memory, testQueueSecret, 1, 1, nil, nil,
		processorTestTimings(time.Millisecond, time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer processor.Close()
	if err := processor.Enqueue(context.Background(), textUpdate(10, "/help")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return flaky.calls.Load() == defaultMaxAttempts })
	time.Sleep(20 * time.Millisecond)
	if flaky.calls.Load() != defaultMaxAttempts {
		t.Fatalf("send attempts = %d, want %d", flaky.calls.Load(), defaultMaxAttempts)
	}
	if _, err := memory.ClaimUpdate(context.Background(), "after-dead", time.Now().Add(time.Hour), time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("dead job was claimable: %v", err)
	}
}

func TestProcessorCloseRejectsNewWorkAndLeavesQueuedWork(t *testing.T) {
	service, _, memory, _, _ := newTestService(t, ai.NewFake())
	processor, err := NewProcessor(context.Background(), service, memory, testQueueSecret, 1, 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	processor.Close()
	if err := processor.Enqueue(context.Background(), textUpdate(22, "after close")); !errors.Is(err, ErrProcessorClosed) {
		t.Fatalf("enqueue after close = %v", err)
	}

	cipher, err := newQueueCipher(testQueueSecret)
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte(`{"update_id":23}`)
	encrypted, err := cipher.seal(23, 42, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, plain) {
		t.Fatal("durable payload contains plaintext")
	}
	if _, err := memory.EnqueueUpdate(context.Background(), domain.UpdateJob{UpdateID: 23, ActorID: 42, Payload: encrypted}); err != nil {
		t.Fatal(err)
	}
	job, err := memory.ClaimUpdate(context.Background(), "restart", time.Now().Add(time.Second), time.Minute)
	if err != nil || job.UpdateID != 23 {
		t.Fatalf("recoverable job = %+v, %v", job, err)
	}
}

func TestProcessorShutdownRequeuesInflightWork(t *testing.T) {
	started := make(chan struct{})
	provider := &countingProvider{fn: func(ctx context.Context, _ domain.GenerationRequest) (domain.GenerationResult, error) {
		close(started)
		<-ctx.Done()
		return domain.GenerationResult{}, ctx.Err()
	}}
	service, _, memory, _, _ := newTestService(t, provider)
	_, _ = memory.UpsertUser(context.Background(), domain.User{TelegramID: 42, Language: "ru"})
	_ = memory.SetConsent(context.Background(), 42, true)
	processor, err := NewProcessor(context.Background(), service, memory, testQueueSecret, 1, 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.Enqueue(context.Background(), textUpdate(24, "in flight")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("in-flight provider did not start")
	}
	processor.Close()
	job, err := memory.ClaimUpdate(context.Background(), "restart", time.Now().Add(time.Hour), time.Minute)
	if err != nil || job.UpdateID != 24 || len(job.Payload) == 0 {
		t.Fatalf("recoverable shutdown job = %+v, %v", job, err)
	}
}

func TestProcessorRenewsLeaseDuringLongRunningHandler(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	provider := &countingProvider{fn: func(ctx context.Context, request domain.GenerationRequest) (domain.GenerationResult, error) {
		startedOnce.Do(func() { close(started) })
		select {
		case <-release:
			return ai.NewFake().Generate(ctx, request)
		case <-ctx.Done():
			return domain.GenerationResult{}, ctx.Err()
		}
	}}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	_, _ = memory.UpsertUser(context.Background(), domain.User{TelegramID: 42, Language: "ru"})
	_ = memory.SetConsent(context.Background(), 42, true)
	metrics := observability.NewMetrics()
	processor, err := NewProcessor(context.Background(), service, memory, testQueueSecret, 2, 2, metrics, nil,
		func(processor *Processor) error {
			processor.lease = 120 * time.Millisecond
			processor.pollInterval = 2 * time.Millisecond
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer processor.Close()
	if err := processor.Enqueue(context.Background(), textUpdate(25, "long running")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("long-running provider did not start")
	}

	// Without a heartbeat the second worker would reclaim this update after
	// 120ms. Keep it running through several lease periods to prove ownership
	// is continuously extended.
	time.Sleep(380 * time.Millisecond)
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("provider calls during renewed lease = %d, want 1", calls)
	}
	close(release)
	waitFor(t, time.Second, func() bool { return len(telegramClient.snapshotMessages()) == 1 })
	waitFor(t, time.Second, func() bool {
		pending, _, statsErr := memory.QueueStats(context.Background(), time.Now())
		return statsErr == nil && pending == 0
	})

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(recorder.Body.String(), "witty_reply_queue_lease_renewals_total ") {
		t.Fatalf("lease-renewal metric missing:\n%s", recorder.Body.String())
	}
}

func TestProcessorCancelsHandlerWhenLeaseRenewalFails(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	provider := &countingProvider{fn: func(ctx context.Context, _ domain.GenerationRequest) (domain.GenerationResult, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return domain.GenerationResult{}, ctx.Err()
	}}
	service, _, memory, _, _ := newTestService(t, provider)
	_, _ = memory.UpsertUser(context.Background(), domain.User{TelegramID: 42, Language: "ru"})
	_ = memory.SetConsent(context.Background(), 42, true)
	failingStore := &renewFailingStore{Store: memory, fail: started}
	metrics := observability.NewMetrics()
	processor, err := NewProcessor(context.Background(), service, failingStore, testQueueSecret, 1, 1, metrics, nil,
		func(processor *Processor) error {
			processor.lease = 90 * time.Millisecond
			processor.pollInterval = 2 * time.Millisecond
			processor.maxAttempts = 1
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer processor.Close()
	if err := processor.Enqueue(context.Background(), textUpdate(26, "renewal fails")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("lease-renewal failure did not cancel provider")
	}
	if failingStore.calls.Load() < 4 {
		t.Fatalf("renewal calls = %d, want ownership checks + failed heartbeat", failingStore.calls.Load())
	}
	waitFor(t, time.Second, func() bool {
		pending, _, statsErr := memory.QueueStats(context.Background(), time.Now())
		return statsErr == nil && pending == 0
	})

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(recorder.Body.String(), "witty_reply_queue_lease_renew_errors_total 1") {
		t.Fatalf("lease-renewal error metric missing:\n%s", recorder.Body.String())
	}
}

func TestNewInputSupersedesFailedGenerationRetry(t *testing.T) {
	provider := &countingProvider{next: ai.NewFake()}
	service, telegramClient, memory, _, _ := newTestService(t, provider)
	flaky := &flakyTelegram{fakeTelegram: telegramClient}
	flaky.failures.Store(1)
	service.telegram = flaky
	_, _ = memory.UpsertUser(context.Background(), domain.User{TelegramID: 42, Language: "ru"})
	_ = memory.SetConsent(context.Background(), 42, true)
	processor, err := NewProcessor(context.Background(), service, memory, testQueueSecret, 1, 2, nil, nil,
		processorTestTimings(2*time.Millisecond, 80*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer processor.Close()
	if err := processor.Enqueue(context.Background(), textUpdate(30, "old generation")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return flaky.calls.Load() == 1 })
	if err := processor.Enqueue(context.Background(), textUpdate(31, "new generation")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return len(telegramClient.snapshotMessages()) == 1 })
	waitFor(t, time.Second, func() bool {
		pending, _, statsErr := memory.QueueStats(context.Background(), time.Now())
		return statsErr == nil && pending == 0
	})
	time.Sleep(160 * time.Millisecond)
	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("provider calls = %d, want old + replacement only", calls)
	}
	if calls := flaky.calls.Load(); calls != 2 {
		t.Fatalf("delivery calls = %d, old retry was delivered", calls)
	}
}

func TestActiveRegistryClosesClaimRegistrationRace(t *testing.T) {
	processor := &Processor{
		active: make(map[string]activeClaim), supersededThrough: make(map[int64]int64),
		metrics: observability.NewMetrics(),
	}
	processor.cancelSuperseded(42, 101)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	unregister := processor.registerActive(domain.UpdateJob{
		UpdateID: 100, ActorID: 42, LeaseToken: "old", Supersedable: true,
	}, cancel)
	defer unregister()
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), store.ErrSuperseded) {
			t.Fatalf("cancellation cause = %v", context.Cause(ctx))
		}
	case <-time.After(time.Second):
		t.Fatal("claim registered after supersession was not cancelled")
	}
}

func TestQueuePolicySupersedesOnlyGenerativeAndCancellationUpdates(t *testing.T) {
	service, _, _, _, codec := newTestService(t, ai.NewFake())
	if policy := service.QueuePolicy(textUpdate(1, "content")); !policy.Supersedable || !policy.Superseding {
		t.Fatalf("content policy = %+v", policy)
	}
	if policy := service.QueuePolicy(textUpdate(2, "/cancel")); policy.Supersedable || !policy.Superseding {
		t.Fatalf("cancel policy = %+v", policy)
	}
	if policy := service.QueuePolicy(textUpdate(3, "/help")); policy != (UpdateQueuePolicy{}) {
		t.Fatalf("help policy = %+v", policy)
	}
	data, err := codec.Encode(session.CallbackPayload{
		Action: session.ActionMore, UserID: 42, InteractionID: 9, Revision: 1, Candidate: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	user := testUser()
	policy := service.QueuePolicy(telegram.Update{UpdateID: 4, CallbackQuery: &telegram.CallbackQuery{ID: "more", From: user, Data: data}})
	if !policy.Supersedable || !policy.Superseding {
		t.Fatalf("refinement policy = %+v", policy)
	}
}

func TestQueueCipherAuthenticatesIdentityAndPayload(t *testing.T) {
	cipher, err := newQueueCipher(testQueueSecret)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := []byte(`{"update_id":7,"message":{"text":"secret"}}`)
	encrypted, err := cipher.seal(7, 42, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := cipher.open(7, 42, encrypted)
	if err != nil || !bytes.Equal(decoded, plaintext) {
		t.Fatalf("open = %q, %v", decoded, err)
	}
	if _, err := cipher.open(7, 99, encrypted); err == nil {
		t.Fatal("actor substitution was accepted")
	}
	tampered := append([]byte(nil), encrypted...)
	tampered[len(tampered)-1] ^= 1
	if _, err := cipher.open(7, 42, tampered); err == nil {
		t.Fatal("ciphertext tampering was accepted")
	}
}

type flakyTelegram struct {
	*fakeTelegram
	failures atomic.Int64
	calls    atomic.Int64
}

type renewFailingStore struct {
	store.Store
	calls atomic.Int64
	fail  <-chan struct{}
}

func (s *renewFailingStore) RenewUpdate(ctx context.Context, updateID int64, leaseToken string, now time.Time, lease time.Duration) error {
	s.calls.Add(1)
	select {
	case <-s.fail:
		return errors.New("test renewal failure")
	default:
		return s.Store.RenewUpdate(ctx, updateID, leaseToken, now, lease)
	}
}

func (f *flakyTelegram) SendMessage(ctx context.Context, params telegram.SendMessageParams) (telegram.Message, error) {
	f.calls.Add(1)
	if f.failures.Add(-1) >= 0 {
		return telegram.Message{}, errors.New("temporary Telegram failure")
	}
	return f.fakeTelegram.SendMessage(ctx, params)
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition not met before timeout")
	}
}

func processorTestTimings(poll, retry time.Duration) ProcessorOption {
	return func(processor *Processor) error {
		processor.pollInterval = poll
		processor.retryBase = retry
		processor.retryMaximum = retry
		return nil
	}
}
