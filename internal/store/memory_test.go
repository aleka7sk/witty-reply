package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
)

func TestMemoryLifecycle(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	now := time.Date(2026, 7, 31, 20, 0, 0, 0, time.FixedZone("Asia/Almaty", 5*60*60))
	memory.now = func() time.Time { return now }

	user, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"})
	if err != nil {
		t.Fatal(err)
	}
	if user.HasConsent() {
		t.Fatal("new user unexpectedly consented")
	}
	if err := memory.SetConsent(ctx, 42, true); err != nil {
		t.Fatal(err)
	}
	user, err = memory.GetUser(ctx, 42)
	if err != nil || !user.HasConsent() {
		t.Fatalf("consent not persisted: %+v, %v", user, err)
	}

	first, err := memory.ConsumeQuota(ctx, 42, 1001, domain.QuotaText, 1, now)
	if err != nil || !first.Allowed || first.Used != 1 {
		t.Fatalf("first quota decision = %+v, %v", first, err)
	}
	repeated, err := memory.ConsumeQuota(ctx, 42, 1001, domain.QuotaText, 99, now.Add(time.Hour))
	if err != nil || !repeated.Allowed || repeated.Used != 1 || repeated.Limit != 1 {
		t.Fatalf("repeated charged decision = %+v, %v", repeated, err)
	}
	second, err := memory.ConsumeQuota(ctx, 42, 1002, domain.QuotaText, 1, now)
	if err != nil || second.Allowed || second.Used != 1 {
		t.Fatalf("second quota decision = %+v, %v", second, err)
	}
	if err := memory.RefundQuota(ctx, 42, 1001, domain.QuotaText); err != nil {
		t.Fatal(err)
	}
	deniedAgain, err := memory.ConsumeQuota(ctx, 42, 1002, domain.QuotaText, 99, now.Add(time.Hour))
	if err != nil || deniedAgain.Allowed || deniedAgain.Used != 1 || deniedAgain.Limit != 1 {
		t.Fatalf("repeated denied decision = %+v, %v", deniedAgain, err)
	}
	afterRefund, err := memory.ConsumeQuota(ctx, 42, 1001, domain.QuotaText, 1, now)
	if err != nil || !afterRefund.Allowed || afterRefund.Used != 1 {
		t.Fatalf("quota after refund = %+v, %v", afterRefund, err)
	}
	if err := memory.RefundQuota(ctx, 42, 1001, domain.QuotaText); err != nil {
		t.Fatal(err)
	}
	if err := memory.RefundQuota(ctx, 42, 1001, domain.QuotaText); err != nil {
		t.Fatalf("duplicate refund: %v", err)
	}
	third, err := memory.ConsumeQuota(ctx, 42, 1003, domain.QuotaText, 1, now)
	if err != nil || !third.Allowed || third.Used != 1 {
		t.Fatalf("quota after idempotent refund = %+v, %v", third, err)
	}

	id, err := memory.SaveGeneration(ctx, domain.GenerationRecord{
		TelegramID: 42, InputKind: domain.InputText, Tone: domain.ToneMix,
		Result: domain.GenerationResult{Replies: []domain.Reply{{Tone: domain.ToneSmart, Text: "Ответ"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.RecordFeedback(ctx, domain.Feedback{GenerationID: id, TelegramID: 99, Rating: 1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign feedback error = %v", err)
	}
	if saved, err := memory.SaveStyleExample(ctx, 42, id, "Ответ", 5); err != nil || !saved {
		t.Fatal(err)
	}
	if err := memory.DeleteUser(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.GetUser(ctx, 42); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted user error = %v", err)
	}
}

func TestMemoryStyleLimitIsAtomicAndDuplicateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	_, _ = memory.UpsertUser(ctx, domain.User{TelegramID: 42})
	ids := make([]int64, 8)
	for index := range ids {
		id, err := memory.SaveGeneration(ctx, domain.GenerationRecord{TelegramID: 42, InputKind: domain.InputText})
		if err != nil {
			t.Fatal(err)
		}
		ids[index] = id
	}
	var workers sync.WaitGroup
	for index, id := range ids {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, _ = memory.SaveStyleExample(ctx, 42, id, fmt.Sprintf("style-%d", index), 3)
		}()
	}
	workers.Wait()
	examples, err := memory.ListStyleExamples(ctx, 42, 20)
	if err != nil || len(examples) != 3 {
		t.Fatalf("examples = %v, %v; want exactly 3", examples, err)
	}
	saved, err := memory.SaveStyleExample(ctx, 42, ids[0], "style-0", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(examples) == 3 && !saved {
		// ids[0] may not have won the concurrent race; idempotency is asserted
		// below against a value that is known to exist.
		known := examples[0]
		saved, err = memory.SaveStyleExample(ctx, 42, ids[0], known, 3)
	}
	if err != nil || !saved {
		t.Fatalf("duplicate save = %v, %v", saved, err)
	}
	after, _ := memory.ListStyleExamples(ctx, 42, 20)
	if len(after) != 3 {
		t.Fatalf("duplicate consumed a slot: %v", after)
	}
}

func TestMemoryDurableUpdateLifecycleAndLeaseRecovery(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	memory.now = func() time.Time { return now }
	inserted, err := memory.EnqueueUpdate(ctx, domain.UpdateJob{UpdateID: 100, ActorID: 42, Payload: []byte("ciphertext")})
	if err != nil || !inserted {
		t.Fatalf("enqueue = %v, %v", inserted, err)
	}
	inserted, err = memory.EnqueueUpdate(ctx, domain.UpdateJob{UpdateID: 100, ActorID: 42, Payload: []byte("duplicate")})
	if err != nil || inserted {
		t.Fatalf("duplicate enqueue = %v, %v", inserted, err)
	}

	first, err := memory.ClaimUpdate(ctx, "lease-1", now, time.Minute)
	if err != nil || first.Attempts != 1 || string(first.Payload) != "ciphertext" {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	if _, err := memory.ClaimUpdate(ctx, "blocked", now.Add(30*time.Second), time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim during lease = %v", err)
	}
	if err := memory.RenewUpdate(ctx, 100, "wrong-token", now.Add(30*time.Second), time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong-token renewal = %v", err)
	}
	if err := memory.RenewUpdate(ctx, 100, "lease-1", now.Add(30*time.Second), time.Minute); err != nil {
		t.Fatalf("renew lease: %v", err)
	}
	if _, err := memory.ClaimUpdate(ctx, "still-blocked", now.Add(time.Minute), time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("claim during renewed lease = %v", err)
	}
	second, err := memory.ClaimUpdate(ctx, "lease-2", now.Add(90*time.Second), time.Minute)
	if err != nil || second.Attempts != 2 {
		t.Fatalf("recovered claim = %+v, %v", second, err)
	}
	if err := memory.CompleteUpdate(ctx, 100, "lease-1", now); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("stale completion = %v", err)
	}
	retryAt := now.Add(2 * time.Minute)
	dead, err := memory.RetryUpdate(ctx, 100, "lease-2", now, retryAt, "temporary", 8)
	if err != nil || dead {
		t.Fatalf("retry = %v, %v", dead, err)
	}
	if _, err := memory.ClaimUpdate(ctx, "early", retryAt.Add(-time.Second), time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("early retry claim = %v", err)
	}
	third, err := memory.ClaimUpdate(ctx, "lease-3", retryAt, time.Minute)
	if err != nil || third.Attempts != 3 {
		t.Fatalf("third claim = %+v, %v", third, err)
	}
	if err := memory.CompleteUpdate(ctx, 100, "lease-3", retryAt); err != nil {
		t.Fatal(err)
	}
	if len(memory.updates[100].Payload) != 0 || memory.updates[100].Status != domain.UpdateJobCompleted {
		t.Fatalf("completed job retained payload: %+v", memory.updates[100])
	}
}

func TestMemorySerializesActorsAndDeadLetters(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	memory.now = func() time.Time { return now }
	for _, job := range []domain.UpdateJob{
		{UpdateID: 1, ActorID: 42, Payload: []byte("one")},
		{UpdateID: 2, ActorID: 42, Payload: []byte("two")},
		{UpdateID: 3, ActorID: 99, Payload: []byte("three")},
	} {
		if inserted, err := memory.EnqueueUpdate(ctx, job); err != nil || !inserted {
			t.Fatalf("enqueue %+v = %v, %v", job, inserted, err)
		}
	}
	first, err := memory.ClaimUpdate(ctx, "first", now, time.Minute)
	if err != nil || first.UpdateID != 1 {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	parallel, err := memory.ClaimUpdate(ctx, "parallel", now, time.Minute)
	if err != nil || parallel.UpdateID != 3 {
		t.Fatalf("parallel claim = %+v, %v", parallel, err)
	}
	if err := memory.CompleteUpdate(ctx, first.UpdateID, first.LeaseToken, now); err != nil {
		t.Fatal(err)
	}
	second, err := memory.ClaimUpdate(ctx, "second", now, time.Minute)
	if err != nil || second.UpdateID != 2 {
		t.Fatalf("second actor claim = %+v, %v", second, err)
	}
	dead, err := memory.RetryUpdate(ctx, second.UpdateID, second.LeaseToken, now, now, "permanent", 1)
	if err != nil || !dead {
		t.Fatalf("dead letter = %v, %v", dead, err)
	}
	if len(memory.updates[2].Payload) != 0 || memory.updates[2].Status != domain.UpdateJobDead {
		t.Fatalf("dead job retained payload: %+v", memory.updates[2])
	}
}

func TestMemorySupersedesOnlyOlderGenerativeJobs(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	now := time.Now().UTC()
	memory.now = func() time.Time { return now }
	for _, job := range []domain.UpdateJob{
		{UpdateID: 10, ActorID: 42, Payload: []byte("old-generation"), Supersedable: true},
		{UpdateID: 11, ActorID: 42, Payload: []byte("help-command")},
	} {
		if inserted, err := memory.EnqueueUpdate(ctx, job); err != nil || !inserted {
			t.Fatalf("enqueue %d = %v, %v", job.UpdateID, inserted, err)
		}
	}
	old, err := memory.ClaimUpdate(ctx, "old-lease", now, time.Minute)
	if err != nil || old.UpdateID != 10 {
		t.Fatalf("claim old generation = %+v, %v", old, err)
	}
	if inserted, err := memory.EnqueueUpdate(ctx, domain.UpdateJob{
		UpdateID: 12, ActorID: 42, Payload: []byte("replacement"), Supersedable: true, Superseding: true,
	}); err != nil || !inserted {
		t.Fatalf("enqueue replacement = %v, %v", inserted, err)
	}
	if got := memory.updates[10]; got.Status != domain.UpdateJobSuperseded || len(got.Payload) != 0 || !got.LeaseUntil.IsZero() {
		t.Fatalf("old generation was not terminally scrubbed: %+v", got)
	}
	if err := memory.RenewUpdate(ctx, old.UpdateID, old.LeaseToken, now, time.Minute); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("superseded renewal = %v", err)
	}
	command, err := memory.ClaimUpdate(ctx, "command-lease", now, time.Minute)
	if err != nil || command.UpdateID != 11 {
		t.Fatalf("non-generative command was skipped: %+v, %v", command, err)
	}
	if err := memory.CompleteUpdate(ctx, command.UpdateID, command.LeaseToken, now); err != nil {
		t.Fatal(err)
	}
	replacement, err := memory.ClaimUpdate(ctx, "replacement-lease", now, time.Minute)
	if err != nil || replacement.UpdateID != 12 || !replacement.Supersedable || !replacement.Superseding {
		t.Fatalf("replacement claim = %+v, %v", replacement, err)
	}
}

func TestMemoryDeleteUserRemovesActorQueueAndFinalizationIsIdempotent(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	now := time.Now().UTC()
	memory.now = func() time.Time { return now }
	_, _ = memory.UpsertUser(ctx, domain.User{TelegramID: 42})
	_, _ = memory.EnqueueUpdate(ctx, domain.UpdateJob{UpdateID: 1, ActorID: 42, Payload: []byte("ciphertext")})
	job, err := memory.ClaimUpdate(ctx, "lease", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.DeleteUser(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if _, exists := memory.updates[1]; exists {
		t.Fatal("actor queue survived deletion")
	}
	if err := memory.CompleteUpdate(ctx, job.UpdateID, job.LeaseToken, now); err != nil {
		t.Fatalf("finalize deleted job = %v", err)
	}
}
