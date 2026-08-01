package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func TestMemoryThreadDraftLifecycleIsOwnedCurrentAndIdempotent(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	memory.now = func() time.Time { return now }
	for _, owner := range []int64{42, 99} {
		if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: owner}); err != nil {
			t.Fatal(err)
		}
	}

	first := testThreadDraft(42, 1, "Первый пост")
	firstID, err := memory.CreateThreadDraft(ctx, first)
	if err != nil {
		t.Fatalf("CreateThreadDraft(): %v", err)
	}
	stored, err := memory.GetThreadDraft(ctx, firstID, 42)
	if err != nil || !stored.Current || stored.State != domain.ThreadDraftReady || stored.Text != first.Text ||
		stored.Objective != domain.ThreadObjectiveLegacy || stored.ScenarioID != "legacy_unspecified" {
		t.Fatalf("stored first = %+v, %v", stored, err)
	}
	if _, err := memory.GetThreadDraft(ctx, firstID, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign GetThreadDraft() = %v", err)
	}

	const claims = 24
	start := make(chan struct{})
	results := make(chan bool, claims)
	var workers sync.WaitGroup
	for index := range claims {
		workers.Add(1)
		go func(claimToken string) {
			defer workers.Done()
			<-start
			_, claimed, claimErr := memory.ClaimThreadDraft(ctx, firstID, 42, 1, claimToken, now, time.Minute)
			if claimErr != nil {
				t.Errorf("ClaimThreadDraft(): %v", claimErr)
			}
			results <- claimed
		}(fmt.Sprintf("claim-%d", index))
	}
	close(start)
	workers.Wait()
	close(results)
	claimedCount := 0
	for claimed := range results {
		if claimed {
			claimedCount++
		}
	}
	if claimedCount != 1 {
		t.Fatalf("successful claims = %d, want 1", claimedCount)
	}
	stored, _ = memory.GetThreadDraft(ctx, firstID, 42)
	firstClaim := stored.ClaimToken
	if err := memory.SetThreadContainer(ctx, firstID, 42, firstClaim, "container-1"); err != nil {
		t.Fatal(err)
	}
	if err := memory.SetThreadContainer(ctx, firstID, 42, firstClaim, "container-1"); err != nil {
		t.Fatalf("idempotent SetThreadContainer(): %v", err)
	}
	if err := memory.SetThreadContainer(ctx, firstID, 42, firstClaim, "different"); !errors.Is(err, ErrThreadDraftState) {
		t.Fatalf("container replacement error = %v", err)
	}
	if err := memory.BeginThreadPublish(ctx, firstID, 42, firstClaim, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := memory.CompleteThreadDraft(ctx, firstID, 42, firstClaim, "post-1", "https://www.threads.net/@belcanto/post/1"); err != nil {
		t.Fatal(err)
	}
	if err := memory.CompleteThreadDraft(ctx, firstID, 42, firstClaim, "post-1", "https://www.threads.net/@belcanto/post/1"); err != nil {
		t.Fatalf("idempotent CompleteThreadDraft(): %v", err)
	}
	stored, _ = memory.GetThreadDraft(ctx, firstID, 42)
	if stored.State != domain.ThreadDraftPublished || stored.PublishedAt == nil || stored.ContainerID != "container-1" {
		t.Fatalf("published first = %+v", stored)
	}

	now = now.Add(time.Minute)
	secondID, err := memory.CreateThreadDraft(ctx, testThreadDraft(42, 2, "Второй пост"))
	if err != nil {
		t.Fatal(err)
	}
	old, _ := memory.GetThreadDraft(ctx, firstID, 42)
	if old.Current {
		t.Fatal("replacement left old draft current")
	}
	if stale, claimed, err := memory.ClaimThreadDraft(ctx, firstID, 42, 1, "stale-claim", now, time.Minute); err != nil || claimed || stale.Current {
		t.Fatalf("stale claim = %+v, %v, %v", stale, claimed, err)
	}
	texts, err := memory.ListRecentThreadTexts(ctx, 42, 2)
	if err != nil || len(texts) != 2 || texts[0] != "Второй пост" || texts[1] != "Первый пост" {
		t.Fatalf("recent texts = %v, %v", texts, err)
	}

	if _, claimed, err := memory.ClaimThreadDraft(ctx, secondID, 42, 2, "second-claim", now, time.Minute); err != nil || !claimed {
		t.Fatalf("claim second = %v, %v", claimed, err)
	}
	if err := memory.SetThreadContainer(ctx, secondID, 42, "second-claim", "container-2"); err != nil {
		t.Fatal(err)
	}
	if err := memory.FailThreadDraft(ctx, secondID, 42, "second-claim", "temporary", false); err != nil {
		t.Fatal(err)
	}
	if retried, claimed, err := memory.ClaimThreadDraft(ctx, secondID, 42, 2, "retry-claim", now, time.Minute); err != nil || !claimed || retried.ErrorCode != "" || retried.ContainerID != "" {
		t.Fatalf("retry claim = %+v, %v, %v", retried, claimed, err)
	}
	if err := memory.SetThreadContainer(ctx, secondID, 42, "retry-claim", "container-2-retry"); err != nil {
		t.Fatal(err)
	}
	if err := memory.BeginThreadPublish(ctx, secondID, 42, "retry-claim", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := memory.FailThreadDraft(ctx, secondID, 42, "retry-claim", "ambiguous", true); err != nil {
		t.Fatal(err)
	}
	unknown, claimed, err := memory.ClaimThreadDraft(ctx, secondID, 42, 2, "unknown-claim", now, time.Minute)
	if err != nil || claimed || unknown.State != domain.ThreadDraftUnknown {
		t.Fatalf("unknown claim = %+v, %v, %v", unknown, claimed, err)
	}

	now = now.Add(time.Minute)
	thirdID, err := memory.CreateThreadDraft(ctx, testThreadDraft(42, 3, "Третий пост"))
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.CancelThreadDraft(ctx, thirdID, 42, 3); err != nil {
		t.Fatal(err)
	}
	cancelled, _ := memory.GetThreadDraft(ctx, thirdID, 42)
	if cancelled.State != domain.ThreadDraftCancelled || cancelled.Current {
		t.Fatalf("cancelled draft = %+v", cancelled)
	}

	if err := memory.DeleteUser(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.GetThreadDraft(ctx, firstID, 42); !errors.Is(err, ErrNotFound) {
		t.Fatalf("draft survived DeleteUser(): %v", err)
	}
}

func TestMemoryThreadMediaLifecycleIsRevisionedOwnedAndReplaySafe(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	memory.now = func() time.Time { return now }
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"}); err != nil {
		t.Fatal(err)
	}
	draftID, err := memory.CreateThreadDraft(ctx, testThreadDraft(42, 1, "Пост с фотографией"))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := memory.SetThreadDraftMediaMode(ctx, draftID, 42, 1, domain.ThreadMediaImagePending)
	if err != nil || pending.Revision != 2 || pending.MediaMode != domain.ThreadMediaImagePending {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	if stale, claimed, err := memory.ClaimThreadDraft(ctx, draftID, 42, 1, "old", now, time.Minute); err != nil || claimed || stale.Revision != 2 {
		t.Fatalf("stale claim = %+v, %v, %v", stale, claimed, err)
	}

	mediaValue := testThreadMedia(42, 100)
	attached, err := memory.AttachThreadDraftMedia(ctx, draftID, 42, pending.Revision, mediaValue)
	if err != nil || attached.Revision != 3 || attached.MediaMode != domain.ThreadMediaImage || attached.MediaID <= 0 {
		t.Fatalf("attached = %+v, %v", attached, err)
	}
	replayed, err := memory.AttachThreadDraftMedia(ctx, draftID, 42, pending.Revision, mediaValue)
	if err != nil || replayed.ID != attached.ID || replayed.MediaID != attached.MediaID || replayed.Revision != attached.Revision {
		t.Fatalf("replayed = %+v, %v", replayed, err)
	}
	byUpdate, err := memory.GetThreadDraftByMediaUpdate(ctx, 42, 100)
	if err != nil || byUpdate.MediaID != attached.MediaID {
		t.Fatalf("by update = %+v, %v", byUpdate, err)
	}
	if _, err := memory.GetThreadMedia(ctx, attached.MediaID, 77); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-owner media read = %v", err)
	}

	claimedDraft, claimed, err := memory.ClaimThreadDraft(ctx, draftID, 42, attached.Revision, "image-claim", now, time.Minute)
	if err != nil || !claimed || claimedDraft.MediaRightsConfirmedAt == nil {
		t.Fatalf("image claim = %+v, %v, %v", claimedDraft, claimed, err)
	}
	if err := memory.FailThreadDraft(ctx, draftID, 42, "image-claim", "test", false); err != nil {
		t.Fatal(err)
	}
	pending, err = memory.SetThreadDraftMediaMode(ctx, draftID, 42, attached.Revision, domain.ThreadMediaImagePending)
	if err != nil || pending.MediaID != attached.MediaID || pending.MediaRightsConfirmedAt != nil {
		t.Fatalf("replacement pending = %+v, %v", pending, err)
	}
	kept, err := memory.SetThreadDraftMediaMode(ctx, draftID, 42, pending.Revision, domain.ThreadMediaImage)
	if err != nil || kept.MediaID != attached.MediaID || kept.MediaMode != domain.ThreadMediaImage {
		t.Fatalf("kept image = %+v, %v", kept, err)
	}
	textOnly, err := memory.SetThreadDraftMediaMode(ctx, draftID, 42, kept.Revision, domain.ThreadMediaText)
	if err != nil || textOnly.MediaID != 0 || textOnly.MediaMode != domain.ThreadMediaText {
		t.Fatalf("text only = %+v, %v", textOnly, err)
	}
	if _, err := memory.GetThreadMedia(ctx, attached.MediaID, 42); !errors.Is(err, ErrNotFound) {
		t.Fatalf("detached media survived without references: %v", err)
	}
}

func TestMemoryThreadMediaReplayCannotAttachToAnotherDraft(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42}); err != nil {
		t.Fatal(err)
	}
	firstID, err := memory.CreateThreadDraft(ctx, testThreadDraft(42, 1, "Первый пост"))
	if err != nil {
		t.Fatal(err)
	}
	firstPending, err := memory.SetThreadDraftMediaMode(ctx, firstID, 42, 1, domain.ThreadMediaImagePending)
	if err != nil {
		t.Fatal(err)
	}
	mediaValue := testThreadMedia(42, 101)
	attached, err := memory.AttachThreadDraftMedia(ctx, firstID, 42, firstPending.Revision, mediaValue)
	if err != nil {
		t.Fatal(err)
	}

	secondID, err := memory.CreateThreadDraft(ctx, testThreadDraft(42, 4, "Второй пост"))
	if err != nil {
		t.Fatal(err)
	}
	secondPending, err := memory.SetThreadDraftMediaMode(ctx, secondID, 42, 4, domain.ThreadMediaImagePending)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.AttachThreadDraftMedia(ctx, secondID, 42, secondPending.Revision, mediaValue); !errors.Is(err, ErrThreadDraftState) {
		t.Fatalf("cross-draft replay = %v, want ErrThreadDraftState", err)
	}
	secondAfter, err := memory.GetThreadDraft(ctx, secondID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if secondAfter.MediaMode != domain.ThreadMediaImagePending || secondAfter.MediaID != 0 || secondAfter.Revision != secondPending.Revision {
		t.Fatalf("cross-draft replay mutated second draft: %+v", secondAfter)
	}
	firstAfter, err := memory.GetThreadDraft(ctx, firstID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if firstAfter.MediaID != attached.MediaID {
		t.Fatalf("cross-draft replay detached first media: %+v", firstAfter)
	}
}

func TestMemoryCreatesLicensedMediaAndDraftAtomically(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42}); err != nil {
		t.Fatal(err)
	}
	draft := testThreadDraft(42, 1, "Какую песню вы узнаете по одному вдоху?")
	draft.MediaMode = domain.ThreadMediaImage
	mediaValue := testPexelsThreadMedia(42)
	draftID, err := memory.CreateThreadDraftWithMedia(ctx, draft, mediaValue)
	if err != nil {
		t.Fatal(err)
	}
	storedDraft, err := memory.GetThreadDraft(ctx, draftID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if storedDraft.MediaMode != domain.ThreadMediaImage || storedDraft.MediaID <= 0 {
		t.Fatalf("draft = %+v", storedDraft)
	}
	storedMedia, err := memory.GetThreadMedia(ctx, storedDraft.MediaID, 42)
	if err != nil {
		t.Fatal(err)
	}
	if storedMedia.SourceKind != domain.ThreadMediaSourcePexels || storedMedia.SourceAuthor != "Lens Author" || storedMedia.SourceUpdateID != 0 {
		t.Fatalf("media = %+v", storedMedia)
	}

	badDraft := testThreadDraft(42, 2, "Невалидный черновик")
	badDraft.MediaMode = domain.ThreadMediaImage
	badDraft.Provider = ""
	if _, err := memory.CreateThreadDraftWithMedia(ctx, badDraft, testPexelsThreadMedia(42)); err == nil {
		t.Fatal("invalid draft and media transaction succeeded")
	}
	manual := testPexelsThreadMediaWithOperation(42, 9090, "909", "manual bytes")
	if _, err := memory.CreateThreadDraftWithMedia(ctx, draft, manual); !errors.Is(err, ErrThreadDraftState) {
		t.Fatalf("manual media bypassed dedicated attach operation: %v", err)
	}
	if len(memory.threadMedia) != 1 {
		t.Fatalf("orphan media count = %d", len(memory.threadMedia))
	}
}

func TestMemoryManualLicensedMediaIsAtomicReplaySafeAndReplaceable(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42}); err != nil {
		t.Fatal(err)
	}
	draft := testThreadDraft(42, 1, "Песня иногда вспоминается раньше названия.")
	draft.PhotoQuery = "vintage microphone close up"
	draftID, err := memory.CreateThreadDraft(ctx, draft)
	if err != nil {
		t.Fatal(err)
	}
	firstMedia := testPexelsThreadMediaWithOperation(42, 5001, "501", "first licensed bytes")
	first, err := memory.AttachLicensedThreadDraftMedia(ctx, draftID, 42, 1, firstMedia)
	if err != nil || first.MediaMode != domain.ThreadMediaImage || first.Revision != 2 || first.MediaID <= 0 {
		t.Fatalf("first licensed attach = %+v, %v", first, err)
	}
	replayed, err := memory.AttachLicensedThreadDraftMedia(ctx, draftID, 42, 1, firstMedia)
	if err != nil || replayed.MediaID != first.MediaID || replayed.Revision != first.Revision {
		t.Fatalf("licensed replay = %+v, %v", replayed, err)
	}
	byOperation, err := memory.GetThreadDraftByMediaAttachUpdate(ctx, 42, 5001)
	if err != nil || byOperation.MediaID != first.MediaID {
		t.Fatalf("licensed operation lookup = %+v, %v", byOperation, err)
	}

	secondMedia := testPexelsThreadMediaWithOperation(42, 5002, "502", "second licensed bytes")
	second, err := memory.AttachLicensedThreadDraftMedia(ctx, draftID, 42, first.Revision, secondMedia)
	if err != nil || second.Revision != first.Revision+1 || second.MediaID == first.MediaID {
		t.Fatalf("licensed replacement = %+v, %v", second, err)
	}
	if _, err := memory.GetThreadMedia(ctx, first.MediaID, 42); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old licensed bytes survived replacement: %v", err)
	}
	if _, err := memory.GetThreadDraftByMediaAttachUpdate(ctx, 42, 5001); !errors.Is(err, ErrThreadDraftState) {
		t.Fatalf("replaced operation lookup = %v, want ErrThreadDraftState", err)
	}

	thirdID, err := memory.CreateThreadDraft(ctx, testThreadDraft(42, 4, "Другой пост"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := memory.AttachLicensedThreadDraftMedia(ctx, thirdID, 42, 4, firstMedia); !errors.Is(err, ErrThreadDraftState) {
		t.Fatalf("cross-draft licensed replay = %v", err)
	}
	third, err := memory.GetThreadDraft(ctx, thirdID, 42)
	if err != nil || third.MediaMode != domain.ThreadMediaText || third.MediaID != 0 || third.Revision != 4 {
		t.Fatalf("cross-draft replay mutated target = %+v, %v", third, err)
	}
}

func TestMemoryThreadDraftLeaseFencesStaleWorkersAndRecoversByPhase(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	now := time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC)
	memory.now = func() time.Time { return now }
	for _, owner := range []int64{42, 99} {
		if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: owner}); err != nil {
			t.Fatal(err)
		}
	}

	draftID, err := memory.CreateThreadDraft(ctx, testThreadDraft(42, 1, "Восстановимый пост"))
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := memory.ClaimThreadDraft(ctx, draftID, 42, 1, "old-worker", now, time.Minute); err != nil || !claimed {
		t.Fatalf("old claim = %v, %v", claimed, err)
	}
	if err := memory.SetThreadContainer(ctx, draftID, 42, "old-worker", "container-reused"); err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Minute)
	recovered, claimed, err := memory.ClaimThreadDraft(ctx, draftID, 42, 1, "new-worker", now, time.Minute)
	if err != nil || !claimed || recovered.ContainerID != "container-reused" || recovered.ClaimToken != "new-worker" {
		t.Fatalf("pre-publish recovery = %+v, %v, %v", recovered, claimed, err)
	}
	if err := memory.BeginThreadPublish(ctx, draftID, 42, "old-worker", now, time.Minute); !errors.Is(err, ErrThreadDraftState) {
		t.Fatalf("stale worker began publish: %v", err)
	}
	if err := memory.BeginThreadPublish(ctx, draftID, 42, "new-worker", now, time.Minute); err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Minute)
	unknown, claimed, err := memory.ClaimThreadDraft(ctx, draftID, 42, 1, "third-worker", now, time.Minute)
	if err != nil || claimed || unknown.State != domain.ThreadDraftUnknown || unknown.PublishStartedAt == nil {
		t.Fatalf("post-attempt recovery = %+v, %v, %v", unknown, claimed, err)
	}
	if err := memory.CompleteThreadDraft(ctx, draftID, 42, "new-worker", "post-duplicate", ""); !errors.Is(err, ErrThreadDraftState) {
		t.Fatalf("expired worker completed after recovery: %v", err)
	}
	if err := memory.ConfirmThreadDraftPublished(ctx, draftID, 42, "container-reused"); err != nil {
		t.Fatalf("status reconciliation: %v", err)
	}
	published, _ := memory.GetThreadDraft(ctx, draftID, 42)
	if published.State != domain.ThreadDraftPublished || published.PublishedAt == nil || published.ClaimToken != "" {
		t.Fatalf("reconciled draft = %+v", published)
	}

	cleanupID, err := memory.CreateThreadDraft(ctx, testThreadDraft(99, 1, "Cleanup recovery"))
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := memory.ClaimThreadDraft(ctx, cleanupID, 99, 1, "cleanup-worker", now, time.Minute); err != nil || !claimed {
		t.Fatalf("cleanup claim = %v, %v", claimed, err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := memory.Cleanup(ctx, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	recoveredByCleanup, _ := memory.GetThreadDraft(ctx, cleanupID, 99)
	if recoveredByCleanup.State != domain.ThreadDraftFailed || recoveredByCleanup.ClaimToken != "" {
		t.Fatalf("cleanup recovery = %+v", recoveredByCleanup)
	}
}

func TestMemoryCleanupRemovesOldThreadDraftsWithoutChangingGenerationCount(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	memory.now = func() time.Time { return now }
	_, _ = memory.UpsertUser(ctx, domain.User{TelegramID: 42})
	oldID, err := memory.CreateThreadDraft(ctx, testThreadDraft(42, 1, "Старый"))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(48 * time.Hour)
	newID, err := memory.CreateThreadDraft(ctx, testThreadDraft(42, 2, "Новый"))
	if err != nil {
		t.Fatal(err)
	}
	old := memory.threadDrafts[oldID]
	old.UpdatedAt = now.Add(-48 * time.Hour)
	memory.threadDrafts[oldID] = old
	deleted, err := memory.Cleanup(ctx, now.Add(-24*time.Hour))
	if err != nil || deleted != 0 {
		t.Fatalf("Cleanup() = %d, %v", deleted, err)
	}
	if _, err := memory.GetThreadDraft(ctx, oldID, 42); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old draft survived cleanup: %v", err)
	}
	if _, err := memory.GetThreadDraft(ctx, newID, 42); err != nil {
		t.Fatalf("fresh draft removed: %v", err)
	}
}

func TestMemoryThreadBriefLifecycleIsDurableReplaySafeAndCreatesOneDraft(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	memory.now = func() time.Time { return now }
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 42, Language: "ru"}); err != nil {
		t.Fatal(err)
	}

	brief, created, err := memory.StartThreadBrief(ctx, 42, 10, domain.ThreadVoiceBelcanto)
	if err != nil || !created || brief.State != domain.ThreadBriefAwaitingGoal || brief.Revision != 1 {
		t.Fatalf("StartThreadBrief() = %+v, %v, %v", brief, created, err)
	}
	replayedStart, created, err := memory.StartThreadBrief(ctx, 42, 10, domain.ThreadVoiceBelcanto)
	if err != nil || created || replayedStart.ID != brief.ID {
		t.Fatalf("replayed StartThreadBrief() = %+v, %v, %v", replayedStart, created, err)
	}

	brief, err = memory.SetThreadBriefObjective(ctx, brief.ID, 42, brief.Revision, domain.ThreadObjectiveReplies)
	if err != nil || brief.State != domain.ThreadBriefAwaitingMaterial || brief.Revision != 2 {
		t.Fatalf("SetThreadBriefObjective() = %+v, %v", brief, err)
	}
	// The same objective keyboard is also used by "change goal" while the
	// brief is awaiting material.
	brief, err = memory.SetThreadBriefObjective(ctx, brief.ID, 42, brief.Revision, domain.ThreadObjectiveTrust)
	if err != nil || brief.Objective != domain.ThreadObjectiveTrust || brief.Revision != 3 {
		t.Fatalf("changed objective = %+v, %v", brief, err)
	}
	material := "Педагог предложил сначала спокойно проговорить сложную строчку, а потом спеть её."
	materialRevision := brief.Revision
	brief, err = memory.SetThreadBriefMaterial(
		ctx, brief.ID, 42, materialRevision, 11, domain.ThreadMaterialText, material,
	)
	if err != nil || brief.State != domain.ThreadBriefMaterialReady || brief.MaterialText != material {
		t.Fatalf("SetThreadBriefMaterial() = %+v, %v", brief, err)
	}
	replayedMaterial, err := memory.SetThreadBriefMaterial(
		ctx, brief.ID, 42, materialRevision, 11, domain.ThreadMaterialText, material,
	)
	if err != nil || replayedMaterial.Revision != brief.Revision {
		t.Fatalf("replayed material = %+v, %v", replayedMaterial, err)
	}

	draftInput := testThreadDraft(42, 1, "Сложную строчку не всегда нужно сразу петь. Иногда сначала достаточно услышать, как она звучит в обычной речи.")
	draftInput.ScenarioID = "teacher_one_move"
	draftInput.GenerationID = "generation-11"
	draftInput.GenerationUpdateID = 11
	createdDraft, draftCreated, err := memory.CreateThreadDraftForBrief(ctx, brief.ID, brief.Revision, draftInput, nil)
	if err != nil || !draftCreated || createdDraft.BriefID != brief.ID ||
		createdDraft.Objective != domain.ThreadObjectiveTrust || createdDraft.State != domain.ThreadDraftReady {
		t.Fatalf("CreateThreadDraftForBrief() = %+v, %v, %v", createdDraft, draftCreated, err)
	}
	replayedDraft, draftCreated, err := memory.CreateThreadDraftForBrief(ctx, brief.ID, brief.Revision, draftInput, nil)
	if err != nil || draftCreated || replayedDraft.ID != createdDraft.ID {
		t.Fatalf("replayed draft = %+v, %v, %v", replayedDraft, draftCreated, err)
	}
	byUpdate, err := memory.GetThreadDraftByGenerationUpdate(ctx, 42, 11)
	if err != nil || byUpdate.ID != createdDraft.ID {
		t.Fatalf("GetThreadDraftByGenerationUpdate() = %+v, %v", byUpdate, err)
	}
	storedBrief, err := memory.GetThreadBrief(ctx, brief.ID, 42)
	if err != nil || storedBrief.State != domain.ThreadBriefDraftReady || storedBrief.Revision != brief.Revision+1 {
		t.Fatalf("draft-ready brief = %+v, %v", storedBrief, err)
	}

	second, created, err := memory.StartThreadBrief(ctx, 42, 12, domain.ThreadVoiceBelcanto)
	if err != nil || !created {
		t.Fatalf("second StartThreadBrief() = %+v, %v, %v", second, created, err)
	}
	staleDraft, err := memory.GetThreadDraft(ctx, createdDraft.ID, 42)
	if err != nil || staleDraft.Current {
		t.Fatalf("new brief did not stale previous draft: %+v, %v", staleDraft, err)
	}
	if err := memory.CancelThreadBrief(ctx, second.ID, 42, second.Revision); err != nil {
		t.Fatal(err)
	}
	cancelled, err := memory.GetThreadBrief(ctx, second.ID, 42)
	if err != nil || cancelled.State != domain.ThreadBriefCancelled || cancelled.Current {
		t.Fatalf("cancelled brief = %+v, %v", cancelled, err)
	}
	if err := memory.CancelThreadBrief(ctx, second.ID, 42, second.Revision); err != nil {
		t.Fatalf("replayed CancelThreadBrief(): %v", err)
	}
}

func TestMemoryThreadBriefNoMaterialAndDeletion(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 77}); err != nil {
		t.Fatal(err)
	}
	brief, _, err := memory.StartThreadBrief(ctx, 77, 20, domain.ThreadVoiceBelcanto)
	if err != nil {
		t.Fatal(err)
	}
	brief, err = memory.SetThreadBriefObjective(ctx, brief.ID, 77, brief.Revision, domain.ThreadObjectiveReach)
	if err != nil {
		t.Fatal(err)
	}
	brief, err = memory.SetThreadBriefMaterial(ctx, brief.ID, 77, brief.Revision, 21, domain.ThreadMaterialNone, "")
	if err != nil || brief.MaterialKind != domain.ThreadMaterialNone || brief.MaterialUpdateID != 21 {
		t.Fatalf("no-material brief = %+v, %v", brief, err)
	}
	if err := memory.DeleteUser(ctx, 77); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.GetThreadBrief(ctx, brief.ID, 77); !errors.Is(err, ErrNotFound) {
		t.Fatalf("brief survived DeleteUser(): %v", err)
	}
}

func TestMemoryThreadBriefFencesInitialCreationAndAllowsCurrentRefinement(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 88}); err != nil {
		t.Fatal(err)
	}
	brief, _, err := memory.StartThreadBrief(ctx, 88, 30, domain.ThreadVoiceBelcanto)
	if err != nil {
		t.Fatal(err)
	}
	brief, err = memory.SetThreadBriefObjective(ctx, brief.ID, 88, brief.Revision, domain.ThreadObjectiveCommunity)
	if err != nil {
		t.Fatal(err)
	}
	brief, err = memory.SetThreadBriefMaterial(
		ctx, brief.ID, 88, brief.Revision, 31, domain.ThreadMaterialText,
		"После занятия ученики остались выбрать песню для общего караоке.",
	)
	if err != nil {
		t.Fatal(err)
	}
	draft := testThreadDraft(88, 1, "После урока никто не спешил домой: выбирали песню, которую споют вместе.")
	draft.BriefID = brief.ID
	draft.Objective = brief.Objective
	draft.ScenarioID = "community_after_class"
	draft.GenerationID = "generation-31"
	draft.GenerationUpdateID = 31
	if _, err := memory.CreateThreadDraft(ctx, draft); !errors.Is(err, ErrThreadBriefState) {
		t.Fatalf("generic initial draft bypass = %v", err)
	}

	wrongVoice := draft
	wrongVoice.BriefID = 0
	wrongVoice.Voice = domain.ThreadVoiceAlisher
	if _, _, err := memory.CreateThreadDraftForBrief(ctx, brief.ID, brief.Revision, wrongVoice, nil); !errors.Is(err, ErrThreadBriefState) {
		t.Fatalf("cross-voice brief draft = %v", err)
	}
	draft.BriefID = 0
	draft.MediaMode = domain.ThreadMediaImage
	mediaValue := testPexelsThreadMedia(88)
	created, wasCreated, err := memory.CreateThreadDraftForBrief(
		ctx, brief.ID, brief.Revision, draft, &mediaValue,
	)
	if err != nil || !wasCreated || created.MediaID <= 0 {
		t.Fatalf("initial draft with media = %+v, %v, %v", created, wasCreated, err)
	}

	refinement := draft
	refinement.BriefID = brief.ID
	refinement.Revision = 2
	refinement.MediaMode = domain.ThreadMediaText
	refinement.MediaID = 0
	refinement.GenerationID = "generation-32"
	refinement.GenerationUpdateID = 32
	refinement.Text = "После урока ученики остались выбрать одну песню для общего караоке. Какую выбрали бы вы?"
	refinementID, err := memory.CreateThreadDraft(ctx, refinement)
	if err != nil {
		t.Fatalf("current draft refinement: %v", err)
	}
	if refined, err := memory.GetThreadDraft(ctx, refinementID, 88); err != nil || refined.BriefID != brief.ID {
		t.Fatalf("stored refinement = %+v, %v", refined, err)
	}

	if _, _, err := memory.StartThreadBrief(ctx, 88, 33, domain.ThreadVoiceBelcanto); err != nil {
		t.Fatal(err)
	}
	refinement.Revision = 3
	refinement.GenerationID = "generation-34"
	refinement.GenerationUpdateID = 34
	if _, err := memory.CreateThreadDraft(ctx, refinement); !errors.Is(err, ErrThreadBriefState) {
		t.Fatalf("stale brief refinement = %v", err)
	}
}

func testThreadDraft(owner int64, revision uint32, text string) domain.ThreadDraft {
	return domain.ThreadDraft{
		TelegramID: owner,
		Voice:      domain.ThreadVoiceBelcanto,
		Goal:       "обсуждение",
		Text:       text,
		Provider:   "fake",
		Model:      "deterministic",
		Revision:   revision,
	}
}

func TestMemoryThreadFinalistsAreDurableAtomicAndOnlySelectionEntersHistory(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 901}); err != nil {
		t.Fatal(err)
	}
	brief, _, err := memory.StartThreadBrief(ctx, 901, 9001, domain.ThreadVoiceBelcanto)
	if err != nil {
		t.Fatal(err)
	}
	brief, err = memory.SetThreadBriefObjective(ctx, brief.ID, 901, brief.Revision, domain.ThreadObjectiveReplies)
	if err != nil {
		t.Fatal(err)
	}
	brief, err = memory.SetThreadBriefMaterial(ctx, brief.ID, 901, brief.Revision, 9002, domain.ThreadMaterialText, "По субботам в Belcanto проходит караоке.")
	if err != nil {
		t.Fatal(err)
	}
	input := testThreadFinalistSet(901, brief.ID, 0, brief.Revision, 1, 9002, "finalists-memory-9002", brief.Objective)
	set, created, err := memory.CreateThreadFinalistSet(ctx, input)
	if err != nil || !created || set.State != domain.ThreadFinalistSetReady || set.SelectedPosition != -1 {
		t.Fatalf("CreateThreadFinalistSet() = %+v, %v, %v", set, created, err)
	}
	currentSet, err := memory.GetCurrentThreadFinalistSet(ctx, 901)
	if err != nil || currentSet.ID != set.ID || len(currentSet.Candidates) != 5 {
		t.Fatalf("GetCurrentThreadFinalistSet() = %+v, %v", currentSet, err)
	}
	if _, err := memory.GetCurrentThreadFinalistSet(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign current finalist lookup = %v", err)
	}
	if _, err := memory.GetCurrentThreadDraft(ctx, 901); !errors.Is(err, ErrNotFound) {
		t.Fatalf("draft existed before choice: %v", err)
	}
	history, err := memory.ListRecentThreadTexts(ctx, 901, 20)
	if err != nil || len(history) != 0 {
		t.Fatalf("unchosen finalists entered history: %v, %v", history, err)
	}
	replayed, created, err := memory.CreateThreadFinalistSet(ctx, input)
	if err != nil || created || replayed.ID != set.ID {
		t.Fatalf("generation replay = %+v, %v, %v", replayed, created, err)
	}
	if _, _, err := memory.SelectThreadFinalist(ctx, set.ID, 901, set.Revision, 4, 9003); !errors.Is(err, ErrThreadFinalistSetState) {
		t.Fatalf("unselectable finalist error = %v", err)
	}
	draft, selected, err := memory.SelectThreadFinalist(ctx, set.ID, 901, set.Revision, 0, 9004)
	if err != nil || !selected || draft.FinalistSetID != set.ID || draft.Text != set.Candidates[0].Text ||
		draft.MediaMode != domain.ThreadMediaImagePending {
		t.Fatalf("SelectThreadFinalist() = %+v, %v, %v", draft, selected, err)
	}
	if _, err := memory.GetCurrentThreadFinalistSet(ctx, 901); !errors.Is(err, ErrNotFound) {
		t.Fatalf("selected finalist set stayed current: %v", err)
	}
	replayedDraft, selected, err := memory.SelectThreadFinalist(ctx, set.ID, 901, set.Revision, 0, 9004)
	if err != nil || selected || replayedDraft.ID != draft.ID {
		t.Fatalf("selection replay = %+v, %v, %v", replayedDraft, selected, err)
	}
	if _, _, err := memory.SelectThreadFinalist(ctx, set.ID, 901, set.Revision, 1, 9005); !errors.Is(err, ErrThreadFinalistSetState) {
		t.Fatalf("second selection error = %v", err)
	}
	history, err = memory.ListRecentThreadTexts(ctx, 901, 20)
	if err != nil || len(history) != 1 || history[0] != draft.Text {
		t.Fatalf("chosen history = %v, %v", history, err)
	}
	storedBrief, err := memory.GetThreadBrief(ctx, brief.ID, 901)
	if err != nil || storedBrief.State != domain.ThreadBriefDraftReady {
		t.Fatalf("selected brief = %+v, %v", storedBrief, err)
	}
}

func TestMemoryThreadFinalistRefinementCancelRestoresOnlyCurrentSource(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: 902}); err != nil {
		t.Fatal(err)
	}
	baseInput := testThreadDraft(902, 1, "Какую песню вы первой выберете в караоке?")
	baseInput.MediaMode = domain.ThreadMediaImagePending
	baseID, err := memory.CreateThreadDraft(ctx, baseInput)
	if err != nil {
		t.Fatal(err)
	}
	setInput := testThreadFinalistSet(902, 0, baseID, 1, 2, 9101, "finalists-memory-9101", domain.ThreadObjectiveReplies)
	setInput.PreserveMediaMode = domain.ThreadMediaImagePending
	set, created, err := memory.CreateThreadFinalistSet(ctx, setInput)
	if err != nil || !created {
		t.Fatalf("refinement set = %+v, %v, %v", set, created, err)
	}
	if _, err := memory.GetCurrentThreadDraft(ctx, 902); !errors.Is(err, ErrNotFound) {
		t.Fatalf("base stayed current during finalist choice: %v", err)
	}
	currentSet, err := memory.GetCurrentThreadFinalistSet(ctx, 902)
	if err != nil || currentSet.ID != set.ID || currentSet.BaseDraftID != baseID {
		t.Fatalf("current refinement finalists = %+v, %v", currentSet, err)
	}
	restored, ok, err := memory.CancelThreadFinalistSet(ctx, set.ID, 902, set.Revision)
	if err != nil || !ok || restored.ID != baseID || !restored.Current {
		t.Fatalf("cancel restore = %+v, %v, %v", restored, ok, err)
	}
	if _, err := memory.GetCurrentThreadFinalistSet(ctx, 902); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancelled refinement set stayed current: %v", err)
	}
	replayed, ok, err := memory.CancelThreadFinalistSet(ctx, set.ID, 902, set.Revision)
	if err != nil || !ok || replayed.ID != baseID {
		t.Fatalf("cancel replay = %+v, %v, %v", replayed, ok, err)
	}
	if _, _, err := memory.StartThreadBrief(ctx, 902, 9102, domain.ThreadVoiceBelcanto); err != nil {
		t.Fatal(err)
	}
	if _, _, err := memory.CancelThreadFinalistSet(ctx, set.ID, 902, set.Revision); !errors.Is(err, ErrThreadFinalistSetState) {
		t.Fatalf("stale cancel replay returned a non-current base: %v", err)
	}
}

func TestMemoryThreadFinalistInitialCancelReplayRejectsNewWorkflow(t *testing.T) {
	ctx := context.Background()
	memory := NewMemory()
	const ownerID = int64(903)
	if _, err := memory.UpsertUser(ctx, domain.User{TelegramID: ownerID}); err != nil {
		t.Fatal(err)
	}
	brief, _, err := memory.StartThreadBrief(ctx, ownerID, 9201, domain.ThreadVoiceBelcanto)
	if err != nil {
		t.Fatal(err)
	}
	brief, err = memory.SetThreadBriefObjective(ctx, brief.ID, ownerID, brief.Revision, domain.ThreadObjectiveReplies)
	if err != nil {
		t.Fatal(err)
	}
	brief, err = memory.SetThreadBriefMaterial(
		ctx, brief.ID, ownerID, brief.Revision, 9202, domain.ThreadMaterialText,
		"Подтверждённая сцена для отменяемой подборки.",
	)
	if err != nil {
		t.Fatal(err)
	}
	input := testThreadFinalistSet(
		ownerID, brief.ID, 0, brief.Revision, 1, 9203,
		"finalists-memory-9203", brief.Objective,
	)
	set, created, err := memory.CreateThreadFinalistSet(ctx, input)
	if err != nil || !created {
		t.Fatalf("initial finalist set = %+v, %v, %v", set, created, err)
	}
	if _, restored, err := memory.CancelThreadFinalistSet(ctx, set.ID, ownerID, set.Revision); err != nil || restored {
		t.Fatalf("initial cancel = %v, %v", restored, err)
	}
	if _, restored, err := memory.CancelThreadFinalistSet(ctx, set.ID, ownerID, set.Revision); err != nil || restored {
		t.Fatalf("immediate initial cancel replay = %v, %v", restored, err)
	}
	newBrief, created, err := memory.StartThreadBrief(ctx, ownerID, 9204, domain.ThreadVoiceBelcanto)
	if err != nil || !created {
		t.Fatalf("new workflow = %+v, %v, %v", newBrief, created, err)
	}
	if _, _, err := memory.CancelThreadFinalistSet(ctx, set.ID, ownerID, set.Revision); !errors.Is(err, ErrThreadFinalistSetState) {
		t.Fatalf("stale initial cancel replay = %v", err)
	}
	current, err := memory.GetCurrentThreadBrief(ctx, ownerID)
	if err != nil || current.ID != newBrief.ID {
		t.Fatalf("new workflow changed by stale cancel: %+v, %v", current, err)
	}
}

func testThreadFinalistSet(
	owner, briefID, baseDraftID int64,
	sourceRevision, targetRevision uint32,
	generationUpdateID int64,
	generationID string,
	objective domain.ThreadObjective,
) domain.ThreadFinalistSet {
	return domain.ThreadFinalistSet{
		TelegramID: owner, BriefID: briefID, BaseDraftID: baseDraftID,
		GenerationID: generationID, GenerationUpdateID: generationUpdateID,
		Voice: domain.ThreadVoiceBelcanto, Objective: objective,
		Provider: "fake", Model: "deterministic", SourceRevision: sourceRevision,
		TargetDraftRevision: targetRevision, SelectedPosition: -1,
		Candidates: []domain.ThreadFinalist{
			{Position: 0, ReviewerID: "A", Goal: "ответы", Objective: objective, ScenarioID: "karaoke_choice", Mechanism: "question", MaterialBasis: "material", Text: "Какую песню вы первой выберете в караоке?", Recommended: true, Selectable: true, VisualMode: domain.ThreadFinalistVisualLicensedPhoto, PhotoSuggested: true, PhotoQuery: "vintage microphone close up"},
			{Position: 1, ReviewerID: "B", Goal: "ответы", Objective: objective, ScenarioID: "song_memory", Mechanism: "memory", MaterialBasis: "material", Text: "Какую песню вы помните не по словам, а по голосу близкого человека?", Selectable: true, VisualMode: domain.ThreadFinalistVisualTextOnly},
			{Position: 2, ReviewerID: "C", Goal: "ответы", Objective: objective, ScenarioID: "astana_playlist", Mechanism: "local", MaterialBasis: "material", Text: "Какая песня лучше всего звучит во время вечерней поездки по Астане?", Selectable: true, VisualMode: domain.ThreadFinalistVisualTextOnly},
			{Position: 3, ReviewerID: "D", Goal: "ответы", Objective: objective, ScenarioID: "small_group", Mechanism: "trust", MaterialBasis: "material", Text: "Что спокойнее для первого занятия: один на один или маленькая группа?", Selectable: true, VisualMode: domain.ThreadFinalistVisualTextOnly},
			{Position: 4, ReviewerID: "E", Goal: "ответы", Objective: objective, ScenarioID: "community_week", Mechanism: "community", MaterialBasis: "material", Text: "К чему вы бы присоединились сначала: караоке, йога или актёрское занятие?", Selectable: false, VisualMode: domain.ThreadFinalistVisualTextOnly},
		},
	}
}

func testThreadMedia(owner, updateID int64) domain.ThreadMedia {
	data := []byte("normalized-jpeg")
	digest := sha256.Sum256(data)
	return domain.ThreadMedia{
		TelegramID: owner, SourceUpdateID: updateID, Data: data, MediaType: "image/jpeg",
		Width: 1_000, Height: 800, Digest: hex.EncodeToString(digest[:]),
		DeliveryKey: fmt.Sprintf("%032x", updateID),
	}
}

func testPexelsThreadMedia(owner int64) domain.ThreadMedia {
	data := []byte("normalized-pexels-jpeg")
	digest := sha256.Sum256(data)
	return domain.ThreadMedia{
		TelegramID: owner, SourceKind: domain.ThreadMediaSourcePexels,
		SourceAssetID: "123", SourcePageURL: "https://www.pexels.com/photo/microphone-123/",
		SourceAuthor: "Lens Author", SourceAuthorURL: "https://www.pexels.com/@lens-author",
		SourceQuery: "vintage microphone close up",
		Data:        data, MediaType: "image/jpeg", Width: 800, Height: 1_000,
		Digest: hex.EncodeToString(digest[:]), DeliveryKey: "fedcba9876543210fedcba9876543210",
	}
}

func testPexelsThreadMediaWithOperation(owner, operationID int64, assetID, content string) domain.ThreadMedia {
	mediaValue := testPexelsThreadMedia(owner)
	mediaValue.AttachUpdateID = operationID
	mediaValue.SourceAssetID = assetID
	mediaValue.SourcePageURL = "https://www.pexels.com/photo/object-" + assetID + "/"
	mediaValue.Data = []byte(content)
	digest := sha256.Sum256(mediaValue.Data)
	mediaValue.Digest = hex.EncodeToString(digest[:])
	mediaValue.DeliveryKey = fmt.Sprintf("%032x", operationID)
	return mediaValue
}
