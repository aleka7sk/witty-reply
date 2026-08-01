package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPostgresIntegration exercises the properties that the memory store cannot
// prove. It is opt-in locally and runs in CI through TEST_DATABASE_URL. Every
// invocation gets an isolated schema, so it never truncates a shared database.
func TestPostgresIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	postgres := newIsolatedPostgres(t, ctx, databaseURL)

	if err := postgres.Migrate(ctx); err != nil {
		t.Fatalf("second idempotent migration: %v", err)
	}
	if err := postgres.Ping(ctx); err != nil {
		t.Fatalf("Ping(): %v", err)
	}

	// Queue availability is written with PostgreSQL's clock. Use the same clock
	// for deterministic claims and keep a small margin for the setup below.
	var now time.Time
	if err := postgres.pool.QueryRow(ctx, `SELECT clock_timestamp() + interval '1 minute'`).Scan(&now); err != nil {
		t.Fatalf("read PostgreSQL clock: %v", err)
	}
	const (
		ownerID      = int64(71001)
		otherID      = int64(71002)
		quotaRetryID = int64(71003)
	)
	for _, telegramID := range []int64{ownerID, otherID, quotaRetryID} {
		if _, err := postgres.UpsertUser(ctx, domain.User{TelegramID: telegramID, Language: "ru", DefaultTone: domain.ToneMix}); err != nil {
			t.Fatalf("UpsertUser(%d): %v", telegramID, err)
		}
	}

	t.Run("concurrent quota is atomic", func(t *testing.T) {
		const (
			attempts = 32
			limit    = 7
		)
		start := make(chan struct{})
		results := make(chan domain.QuotaDecision, attempts)
		errorsFound := make(chan error, attempts)
		var workers sync.WaitGroup
		workers.Add(attempts)
		for index := range attempts {
			go func() {
				defer workers.Done()
				<-start
				decision, err := postgres.ConsumeQuota(ctx, ownerID, int64(1000+index), domain.QuotaText, limit, now)
				if err != nil {
					errorsFound <- err
					return
				}
				results <- decision
			}()
		}
		close(start)
		workers.Wait()
		close(results)
		close(errorsFound)
		for err := range errorsFound {
			t.Errorf("ConsumeQuota(): %v", err)
		}
		allowed := 0
		seen := 0
		for decision := range results {
			seen++
			if decision.Used < 1 || decision.Used > limit || decision.Limit != limit {
				t.Errorf("invalid quota decision: %+v", decision)
			}
			if decision.Allowed {
				allowed++
			}
		}
		if seen != attempts || allowed != limit {
			t.Fatalf("decisions=%d allowed=%d, want %d and %d", seen, allowed, attempts, limit)
		}
	})

	t.Run("quota reservation is retry and refund idempotent", func(t *testing.T) {
		first, err := postgres.ConsumeQuota(ctx, quotaRetryID, 2001, domain.QuotaText, 1, now)
		if err != nil || !first.Allowed || first.Used != 1 {
			t.Fatalf("first decision = %+v, %v", first, err)
		}
		repeated, err := postgres.ConsumeQuota(ctx, quotaRetryID, 2001, domain.QuotaText, 99, now.Add(time.Hour))
		if err != nil || !repeated.Allowed || repeated.Used != 1 || repeated.Limit != 1 {
			t.Fatalf("charged retry = %+v, %v", repeated, err)
		}
		denied, err := postgres.ConsumeQuota(ctx, quotaRetryID, 2002, domain.QuotaText, 1, now)
		if err != nil || denied.Allowed || denied.Used != 1 {
			t.Fatalf("denied decision = %+v, %v", denied, err)
		}
		if err := postgres.RefundQuota(ctx, quotaRetryID, 2001, domain.QuotaText); err != nil {
			t.Fatal(err)
		}
		deniedAgain, err := postgres.ConsumeQuota(ctx, quotaRetryID, 2002, domain.QuotaText, 99, now.Add(time.Hour))
		if err != nil || deniedAgain.Allowed || deniedAgain.Used != 1 || deniedAgain.Limit != 1 {
			t.Fatalf("denied retry = %+v, %v", deniedAgain, err)
		}
		recharged, err := postgres.ConsumeQuota(ctx, quotaRetryID, 2001, domain.QuotaText, 1, now)
		if err != nil || !recharged.Allowed || recharged.Used != 1 {
			t.Fatalf("refunded retry = %+v, %v", recharged, err)
		}
		if err := postgres.RefundQuota(ctx, quotaRetryID, 2001, domain.QuotaText); err != nil {
			t.Fatal(err)
		}
		if err := postgres.RefundQuota(ctx, quotaRetryID, 2001, domain.QuotaText); err != nil {
			t.Fatalf("duplicate refund: %v", err)
		}
		afterRefund, err := postgres.ConsumeQuota(ctx, quotaRetryID, 2003, domain.QuotaText, 1, now)
		if err != nil || !afterRefund.Allowed || afterRefund.Used != 1 {
			t.Fatalf("decision after duplicate refund = %+v, %v", afterRefund, err)
		}
	})

	generationID, err := postgres.SaveGeneration(ctx, integrationGeneration(ownerID, "owner-digest"))
	if err != nil {
		t.Fatalf("SaveGeneration(): %v", err)
	}

	t.Run("generation ownership is enforced", func(t *testing.T) {
		stored, err := postgres.GetGeneration(ctx, generationID, ownerID)
		if err != nil {
			t.Fatalf("owner GetGeneration(): %v", err)
		}
		if stored.Result.Mode != domain.ScenarioComment || stored.Result.ModeConfidence != domain.ModeConfidenceHigh {
			t.Fatalf("scenario result did not round-trip: %+v", stored.Result)
		}
		if _, err := postgres.GetGeneration(ctx, generationID, otherID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign GetGeneration() error = %v", err)
		}
		if err := postgres.RecordFeedback(ctx, domain.Feedback{GenerationID: generationID, TelegramID: otherID, Candidate: 0, Rating: 1, Action: "rating"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign RecordFeedback() error = %v", err)
		}
		if _, err := postgres.SaveStyleExample(ctx, otherID, generationID, "foreign", 5); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign SaveStyleExample() error = %v", err)
		}
	})

	t.Run("style limit is atomic and duplicate-safe", func(t *testing.T) {
		const styleUserID = int64(71004)
		if _, err := postgres.UpsertUser(ctx, domain.User{TelegramID: styleUserID, Language: "ru", DefaultTone: domain.ToneMix}); err != nil {
			t.Fatalf("UpsertUser(): %v", err)
		}
		generationIDs := make([]int64, 8)
		for index := range generationIDs {
			id, err := postgres.SaveGeneration(ctx, integrationGeneration(styleUserID, fmt.Sprintf("style-digest-%d", index)))
			if err != nil {
				t.Fatalf("SaveGeneration(): %v", err)
			}
			generationIDs[index] = id
		}
		start := make(chan struct{})
		var workers sync.WaitGroup
		for index, id := range generationIDs {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				_, _ = postgres.SaveStyleExample(ctx, styleUserID, id, fmt.Sprintf("style-%d", index), 3)
			}()
		}
		close(start)
		workers.Wait()
		examples, err := postgres.ListStyleExamples(ctx, styleUserID, 20)
		if err != nil || len(examples) != 3 {
			t.Fatalf("examples = %v, %v; want exactly 3", examples, err)
		}
		saved, err := postgres.SaveStyleExample(ctx, styleUserID, generationIDs[0], examples[0], 3)
		if err != nil || !saved {
			t.Fatalf("duplicate save = %v, %v", saved, err)
		}
	})

	t.Run("thread claim binds lease expiry as timestamptz", func(t *testing.T) {
		const claimOwnerID = int64(71006)
		if _, err := postgres.UpsertUser(ctx, domain.User{TelegramID: claimOwnerID}); err != nil {
			t.Fatal(err)
		}
		draftID, err := postgres.CreateThreadDraft(ctx, testThreadDraft(claimOwnerID, 1, "Timestamp claim regression"))
		if err != nil {
			t.Fatal(err)
		}
		var claimAt time.Time
		if err := postgres.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&claimAt); err != nil {
			t.Fatal(err)
		}
		const lease = 37 * time.Second
		claimedDraft, claimed, err := postgres.ClaimThreadDraft(
			ctx, draftID, claimOwnerID, 1, "timestamp-claim", claimAt, lease,
		)
		if err != nil {
			t.Fatalf("ClaimThreadDraft() must bind claim_expires_at as timestamptz: %v", err)
		}
		if !claimed || claimedDraft.ClaimExpiresAt == nil {
			t.Fatalf("claim = %+v, claimed=%v", claimedDraft, claimed)
		}
		wantExpiry := claimAt.UTC().Add(lease)
		if !claimedDraft.ClaimExpiresAt.Equal(wantExpiry) {
			t.Fatalf("claim expiry = %s, want %s", claimedDraft.ClaimExpiresAt, wantExpiry)
		}
		if err := postgres.FailThreadDraft(ctx, draftID, claimOwnerID, "timestamp-claim", "test_complete", false); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("thread drafts are durable owned current and publish idempotent", func(t *testing.T) {
		firstID, err := postgres.CreateThreadDraft(ctx, testThreadDraft(ownerID, 1, "Первый Threads-пост"))
		if err != nil {
			t.Fatalf("CreateThreadDraft(): %v", err)
		}
		first, err := postgres.GetThreadDraft(ctx, firstID, ownerID)
		if err != nil || !first.Current || first.State != domain.ThreadDraftReady || first.Text != "Первый Threads-пост" {
			t.Fatalf("first draft = %+v, %v", first, err)
		}
		if _, err := postgres.GetThreadDraft(ctx, firstID, otherID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign GetThreadDraft() = %v", err)
		}

		const attempts = 24
		start := make(chan struct{})
		results := make(chan bool, attempts)
		errorsFound := make(chan error, attempts)
		var workers sync.WaitGroup
		for index := range attempts {
			workers.Add(1)
			go func(claimToken string) {
				defer workers.Done()
				<-start
				_, claimed, claimErr := postgres.ClaimThreadDraft(ctx, firstID, ownerID, 1, claimToken, now, 5*time.Minute)
				if claimErr != nil {
					errorsFound <- claimErr
					return
				}
				results <- claimed
			}(fmt.Sprintf("claim-%d", index))
		}
		close(start)
		workers.Wait()
		close(results)
		close(errorsFound)
		for claimErr := range errorsFound {
			t.Errorf("ClaimThreadDraft(): %v", claimErr)
		}
		claimedCount := 0
		for claimed := range results {
			if claimed {
				claimedCount++
			}
		}
		if claimedCount != 1 {
			t.Fatalf("successful publish claims = %d, want 1", claimedCount)
		}
		first, _ = postgres.GetThreadDraft(ctx, firstID, ownerID)
		firstClaim := first.ClaimToken
		if err := postgres.SetThreadContainer(ctx, firstID, ownerID, firstClaim, "container-1"); err != nil {
			t.Fatal(err)
		}
		if err := postgres.SetThreadContainer(ctx, firstID, ownerID, firstClaim, "container-1"); err != nil {
			t.Fatalf("idempotent SetThreadContainer(): %v", err)
		}
		if err := postgres.BeginThreadPublish(ctx, firstID, ownerID, firstClaim, now, 5*time.Minute); err != nil {
			t.Fatal(err)
		}
		permalink := "https://www.threads.net/@belcanto/post/1"
		if err := postgres.CompleteThreadDraft(ctx, firstID, ownerID, firstClaim, "post-1", permalink); err != nil {
			t.Fatal(err)
		}
		if err := postgres.CompleteThreadDraft(ctx, firstID, ownerID, firstClaim, "post-1", permalink); err != nil {
			t.Fatalf("idempotent CompleteThreadDraft(): %v", err)
		}
		first, err = postgres.GetThreadDraft(ctx, firstID, ownerID)
		if err != nil || first.State != domain.ThreadDraftPublished || first.PublishedAt == nil || first.ContainerID != "container-1" {
			t.Fatalf("published draft = %+v, %v", first, err)
		}
		restartedPool, err := pgxpool.NewWithConfig(ctx, postgres.pool.Config())
		if err != nil {
			t.Fatalf("reopen PostgreSQL pool: %v", err)
		}
		restarted := &Postgres{pool: restartedPool}
		reloaded, reloadErr := restarted.GetThreadDraft(ctx, firstID, ownerID)
		restarted.Close()
		if reloadErr != nil || reloaded.State != domain.ThreadDraftPublished || reloaded.PostID != "post-1" {
			t.Fatalf("draft after store restart = %+v, %v", reloaded, reloadErr)
		}

		secondID, err := postgres.CreateThreadDraft(ctx, testThreadDraft(ownerID, 2, "Второй Threads-пост"))
		if err != nil {
			t.Fatal(err)
		}
		first, _ = postgres.GetThreadDraft(ctx, firstID, ownerID)
		if first.Current {
			t.Fatal("replacement left the published draft current")
		}
		stale, claimed, err := postgres.ClaimThreadDraft(ctx, firstID, ownerID, 1, "stale-claim", now, 5*time.Minute)
		if err != nil || claimed || stale.Current {
			t.Fatalf("stale claim = %+v, %v, %v", stale, claimed, err)
		}
		texts, err := postgres.ListRecentThreadTexts(ctx, ownerID, 2)
		if err != nil || len(texts) != 2 || texts[0] != "Второй Threads-пост" || texts[1] != "Первый Threads-пост" {
			t.Fatalf("recent texts = %v, %v", texts, err)
		}
		if _, claimed, err := postgres.ClaimThreadDraft(ctx, secondID, ownerID, 2, "second-claim", now, 5*time.Minute); err != nil || !claimed {
			t.Fatalf("claim second = %v, %v", claimed, err)
		}
		if err := postgres.SetThreadContainer(ctx, secondID, ownerID, "second-claim", "container-2"); err != nil {
			t.Fatal(err)
		}
		if err := postgres.FailThreadDraft(ctx, secondID, ownerID, "second-claim", "temporary", false); err != nil {
			t.Fatal(err)
		}
		if retried, claimed, err := postgres.ClaimThreadDraft(ctx, secondID, ownerID, 2, "retry-claim", now, 5*time.Minute); err != nil || !claimed || retried.ErrorCode != "" || retried.ContainerID != "" {
			t.Fatalf("retry claim = %+v, %v, %v", retried, claimed, err)
		}
		if err := postgres.SetThreadContainer(ctx, secondID, ownerID, "retry-claim", "container-2-retry"); err != nil {
			t.Fatal(err)
		}
		if err := postgres.BeginThreadPublish(ctx, secondID, ownerID, "retry-claim", now, 5*time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := postgres.FailThreadDraft(ctx, secondID, ownerID, "retry-claim", "ambiguous", true); err != nil {
			t.Fatal(err)
		}
		unknown, claimed, err := postgres.ClaimThreadDraft(ctx, secondID, ownerID, 2, "unknown-claim", now, 5*time.Minute)
		if err != nil || claimed || unknown.State != domain.ThreadDraftUnknown {
			t.Fatalf("unknown claim = %+v, %v, %v", unknown, claimed, err)
		}

		thirdID, err := postgres.CreateThreadDraft(ctx, testThreadDraft(ownerID, 3, "Третий Threads-пост"))
		if err != nil {
			t.Fatal(err)
		}
		if err := postgres.CancelThreadDraft(ctx, thirdID, ownerID, 3); err != nil {
			t.Fatal(err)
		}
		cancelled, err := postgres.GetThreadDraft(ctx, thirdID, ownerID)
		if err != nil || cancelled.State != domain.ThreadDraftCancelled || cancelled.Current {
			t.Fatalf("cancelled draft = %+v, %v", cancelled, err)
		}
	})

	t.Run("thread publish leases fence stale workers and recover by phase", func(t *testing.T) {
		const recoveryOwner = int64(71004)
		if _, err := postgres.UpsertUser(ctx, domain.User{TelegramID: recoveryOwner}); err != nil {
			t.Fatal(err)
		}
		var claimNow time.Time
		if err := postgres.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&claimNow); err != nil {
			t.Fatal(err)
		}
		draftID, err := postgres.CreateThreadDraft(ctx, testThreadDraft(recoveryOwner, 1, "Recovery post"))
		if err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := postgres.ClaimThreadDraft(ctx, draftID, recoveryOwner, 1, "old-worker", claimNow, 5*time.Minute); err != nil || !claimed {
			t.Fatalf("old claim = %v, %v", claimed, err)
		}
		if err := postgres.SetThreadContainer(ctx, draftID, recoveryOwner, "old-worker", "recovery-container"); err != nil {
			t.Fatal(err)
		}
		if _, err := postgres.pool.Exec(ctx, `UPDATE thread_drafts SET claim_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, draftID); err != nil {
			t.Fatal(err)
		}
		if err := postgres.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&claimNow); err != nil {
			t.Fatal(err)
		}
		recovered, claimed, err := postgres.ClaimThreadDraft(ctx, draftID, recoveryOwner, 1, "new-worker", claimNow, 5*time.Minute)
		if err != nil || !claimed || recovered.ContainerID != "recovery-container" {
			t.Fatalf("pre-publish recovery = %+v, %v, %v", recovered, claimed, err)
		}
		if err := postgres.BeginThreadPublish(ctx, draftID, recoveryOwner, "old-worker", claimNow, 5*time.Minute); !errors.Is(err, ErrThreadDraftState) {
			t.Fatalf("stale worker began publish: %v", err)
		}
		if err := postgres.BeginThreadPublish(ctx, draftID, recoveryOwner, "new-worker", claimNow, 5*time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err := postgres.pool.Exec(ctx, `UPDATE thread_drafts SET claim_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, draftID); err != nil {
			t.Fatal(err)
		}
		if err := postgres.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&claimNow); err != nil {
			t.Fatal(err)
		}
		unknown, claimed, err := postgres.ClaimThreadDraft(ctx, draftID, recoveryOwner, 1, "third-worker", claimNow, 5*time.Minute)
		if err != nil || claimed || unknown.State != domain.ThreadDraftUnknown || unknown.PublishStartedAt == nil {
			t.Fatalf("post-attempt recovery = %+v, %v, %v", unknown, claimed, err)
		}
		if err := postgres.CompleteThreadDraft(ctx, draftID, recoveryOwner, "new-worker", "duplicate", ""); !errors.Is(err, ErrThreadDraftState) {
			t.Fatalf("expired worker completed after recovery: %v", err)
		}
		if err := postgres.ConfirmThreadDraftPublished(ctx, draftID, recoveryOwner, "recovery-container"); err != nil {
			t.Fatalf("status reconciliation: %v", err)
		}
		published, err := postgres.GetThreadDraft(ctx, draftID, recoveryOwner)
		if err != nil || published.State != domain.ThreadDraftPublished || published.PublishedAt == nil || published.ClaimToken != "" {
			t.Fatalf("reconciled = %+v, %v", published, err)
		}
	})

	t.Run("thread media is revisioned owned replay safe and deleted", func(t *testing.T) {
		const mediaOwner = int64(71006)
		if _, err := postgres.UpsertUser(ctx, domain.User{TelegramID: mediaOwner, Language: "ru"}); err != nil {
			t.Fatal(err)
		}
		draftID, err := postgres.CreateThreadDraft(ctx, testThreadDraft(mediaOwner, 1, "Пост с фотографией"))
		if err != nil {
			t.Fatal(err)
		}
		pending, err := postgres.SetThreadDraftMediaMode(ctx, draftID, mediaOwner, 1, domain.ThreadMediaImagePending)
		if err != nil || pending.Revision != 2 {
			t.Fatalf("pending = %+v, %v", pending, err)
		}
		mediaValue := testThreadMedia(mediaOwner, 88001)
		attached, err := postgres.AttachThreadDraftMedia(ctx, draftID, mediaOwner, pending.Revision, mediaValue)
		if err != nil || attached.MediaMode != domain.ThreadMediaImage || attached.MediaID <= 0 || attached.Revision != 3 {
			t.Fatalf("attached = %+v, %v", attached, err)
		}
		replayed, err := postgres.AttachThreadDraftMedia(ctx, draftID, mediaOwner, pending.Revision, mediaValue)
		if err != nil || replayed.MediaID != attached.MediaID || replayed.Revision != attached.Revision {
			t.Fatalf("replayed = %+v, %v", replayed, err)
		}
		if _, err := postgres.GetThreadMedia(ctx, attached.MediaID, otherID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-owner media read = %v", err)
		}
		byKey, err := postgres.GetThreadMediaByDeliveryKey(ctx, mediaValue.DeliveryKey)
		if err != nil || byKey.ID != attached.MediaID || byKey.SourceUpdateID != mediaValue.SourceUpdateID {
			t.Fatalf("delivery lookup = %+v, %v", byKey, err)
		}
		claimedDraft, claimed, err := postgres.ClaimThreadDraft(ctx, draftID, mediaOwner, attached.Revision, "media-claim", now, 5*time.Minute)
		if err != nil || !claimed || claimedDraft.MediaRightsConfirmedAt == nil {
			t.Fatalf("media claim = %+v, %v, %v", claimedDraft, claimed, err)
		}
		if err := postgres.FailThreadDraft(ctx, draftID, mediaOwner, "media-claim", "test", false); err != nil {
			t.Fatal(err)
		}
		pending, err = postgres.SetThreadDraftMediaMode(ctx, draftID, mediaOwner, attached.Revision, domain.ThreadMediaImagePending)
		if err != nil || pending.MediaID != attached.MediaID || pending.MediaRightsConfirmedAt != nil {
			t.Fatalf("replacement pending = %+v, %v", pending, err)
		}
		kept, err := postgres.SetThreadDraftMediaMode(ctx, draftID, mediaOwner, pending.Revision, domain.ThreadMediaImage)
		if err != nil || kept.MediaID != attached.MediaID {
			t.Fatalf("kept image = %+v, %v", kept, err)
		}
		textOnly, err := postgres.SetThreadDraftMediaMode(ctx, draftID, mediaOwner, kept.Revision, domain.ThreadMediaText)
		if err != nil || textOnly.MediaID != 0 {
			t.Fatalf("text only = %+v, %v", textOnly, err)
		}
		if _, err := postgres.GetThreadMedia(ctx, attached.MediaID, mediaOwner); !errors.Is(err, ErrNotFound) {
			t.Fatalf("orphan media survived detach: %v", err)
		}
		if err := postgres.DeleteUser(ctx, mediaOwner); err != nil {
			t.Fatal(err)
		}
		var drafts, mediaRows int
		if err := postgres.pool.QueryRow(ctx, `
			SELECT
				(SELECT count(*) FROM thread_drafts WHERE telegram_id = $1),
				(SELECT count(*) FROM thread_media WHERE telegram_id = $1)`, mediaOwner).Scan(&drafts, &mediaRows); err != nil {
			t.Fatal(err)
		}
		if drafts != 0 || mediaRows != 0 {
			t.Fatalf("delete user left drafts=%d media=%d", drafts, mediaRows)
		}
	})

	t.Run("thread media stale fallbacks use one connection and never cross drafts", func(t *testing.T) {
		const mediaOwner = int64(71007)
		if _, err := postgres.UpsertUser(ctx, domain.User{TelegramID: mediaOwner}); err != nil {
			t.Fatal(err)
		}
		firstID, err := postgres.CreateThreadDraft(ctx, testThreadDraft(mediaOwner, 1, "Первый media draft"))
		if err != nil {
			t.Fatal(err)
		}
		firstPending, err := postgres.SetThreadDraftMediaMode(ctx, firstID, mediaOwner, 1, domain.ThreadMediaImagePending)
		if err != nil {
			t.Fatal(err)
		}
		firstMedia := testThreadMedia(mediaOwner, 88101)
		firstAttached, err := postgres.AttachThreadDraftMedia(ctx, firstID, mediaOwner, firstPending.Revision, firstMedia)
		if err != nil {
			t.Fatal(err)
		}
		secondID, err := postgres.CreateThreadDraft(ctx, testThreadDraft(mediaOwner, 4, "Второй media draft"))
		if err != nil {
			t.Fatal(err)
		}
		secondPending, err := postgres.SetThreadDraftMediaMode(ctx, secondID, mediaOwner, 4, domain.ThreadMediaImagePending)
		if err != nil {
			t.Fatal(err)
		}

		singleConfig := postgres.pool.Config()
		singleConfig.MinConns = 0
		singleConfig.MaxConns = 1
		singlePool, err := pgxpool.NewWithConfig(ctx, singleConfig)
		if err != nil {
			t.Fatal(err)
		}
		defer singlePool.Close()
		single := &Postgres{pool: singlePool}
		fallbackCtx, fallbackCancel := context.WithTimeout(ctx, 2*time.Second)
		defer fallbackCancel()

		if _, err := single.SetThreadDraftMediaMode(
			fallbackCtx, secondID, mediaOwner, secondPending.Revision-1, domain.ThreadMediaText,
		); !errors.Is(err, ErrThreadDraftState) {
			t.Fatalf("single-connection stale mode fallback = %v", err)
		}
		newButStale := testThreadMedia(mediaOwner, 88102)
		if _, err := single.AttachThreadDraftMedia(
			fallbackCtx, secondID, mediaOwner, secondPending.Revision-1, newButStale,
		); !errors.Is(err, ErrThreadDraftState) {
			t.Fatalf("single-connection stale attach fallback = %v", err)
		}
		if _, err := single.AttachThreadDraftMedia(
			fallbackCtx, secondID, mediaOwner, secondPending.Revision, firstMedia,
		); !errors.Is(err, ErrThreadDraftState) {
			t.Fatalf("cross-draft media replay = %v", err)
		}
		secondAfter, err := postgres.GetThreadDraft(ctx, secondID, mediaOwner)
		if err != nil {
			t.Fatal(err)
		}
		if secondAfter.MediaMode != domain.ThreadMediaImagePending || secondAfter.MediaID != 0 ||
			secondAfter.Revision != secondPending.Revision {
			t.Fatalf("stale fallback mutated second draft: %+v", secondAfter)
		}
		firstAfter, err := postgres.GetThreadDraft(ctx, firstID, mediaOwner)
		if err != nil {
			t.Fatal(err)
		}
		if firstAfter.MediaID != firstAttached.MediaID {
			t.Fatalf("cross-draft replay detached first media: %+v", firstAfter)
		}
	})

	t.Run("concurrent thread replacements leave one current draft", func(t *testing.T) {
		const attempts = 12
		start := make(chan struct{})
		errorsFound := make(chan error, attempts)
		var workers sync.WaitGroup
		for index := range attempts {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				_, err := postgres.CreateThreadDraft(ctx, testThreadDraft(
					otherID, uint32(index+1), fmt.Sprintf("Параллельный пост %d", index+1),
				))
				if err != nil {
					errorsFound <- err
				}
			}()
		}
		close(start)
		workers.Wait()
		close(errorsFound)
		for err := range errorsFound {
			t.Errorf("CreateThreadDraft(): %v", err)
		}
		var total, current int
		if err := postgres.pool.QueryRow(ctx, `
			SELECT count(*), count(*) FILTER (WHERE is_current)
			FROM thread_drafts WHERE telegram_id = $1`, otherID).Scan(&total, &current); err != nil {
			t.Fatal(err)
		}
		if total != attempts || current != 1 {
			t.Fatalf("thread replacements total=%d current=%d, want %d and 1", total, current, attempts)
		}
	})

	t.Run("durable inbox serializes actors and recovers leases", func(t *testing.T) {
		jobs := []domain.UpdateJob{
			{UpdateID: 71701, ActorID: 71700, Payload: []byte("encrypted-one")},
			{UpdateID: 71702, ActorID: 71700, Payload: []byte("encrypted-two")},
			{UpdateID: 71703, ActorID: 71799, Payload: []byte("encrypted-three")},
		}
		for _, job := range jobs {
			if inserted, err := postgres.EnqueueUpdate(ctx, job); err != nil || !inserted {
				t.Fatalf("EnqueueUpdate(%d) = %v, %v", job.UpdateID, inserted, err)
			}
		}
		first, err := postgres.ClaimUpdate(ctx, "first-lease", now, time.Minute)
		if err != nil || first.UpdateID != 71701 {
			t.Fatalf("first claim = %+v, %v", first, err)
		}
		if err := postgres.RenewUpdate(ctx, first.UpdateID, "wrong-lease", now.Add(10*time.Second), time.Minute); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("wrong-token renewal = %v", err)
		}
		if err := postgres.RenewUpdate(ctx, first.UpdateID, first.LeaseToken, now.Add(10*time.Second), time.Minute); err != nil {
			t.Fatalf("renew first lease: %v", err)
		}
		parallel, err := postgres.ClaimUpdate(ctx, "parallel-lease", now, time.Minute)
		if err != nil || parallel.UpdateID != 71703 {
			t.Fatalf("parallel claim = %+v, %v", parallel, err)
		}
		if err := postgres.CompleteUpdate(ctx, first.UpdateID, first.LeaseToken, now); err != nil {
			t.Fatalf("complete first: %v", err)
		}
		if err := postgres.CompleteUpdate(ctx, parallel.UpdateID, parallel.LeaseToken, now); err != nil {
			t.Fatalf("complete parallel: %v", err)
		}
		second, err := postgres.ClaimUpdate(ctx, "second-lease", now, time.Minute)
		if err != nil || second.UpdateID != 71702 {
			t.Fatalf("second actor claim = %+v, %v", second, err)
		}
		dead, err := postgres.RetryUpdate(ctx, second.UpdateID, second.LeaseToken, now, now, "permanent", 1)
		if err != nil || !dead {
			t.Fatalf("dead letter = %v, %v", dead, err)
		}
		var payloadLength int
		if err := postgres.pool.QueryRow(ctx, `SELECT octet_length(payload) FROM telegram_update_jobs WHERE update_id = $1`, second.UpdateID).Scan(&payloadLength); err != nil {
			t.Fatalf("dead payload length: %v", err)
		}
		if payloadLength != 0 {
			t.Fatalf("dead payload retained %d bytes", payloadLength)
		}

		const staleID int64 = 71704
		if inserted, err := postgres.EnqueueUpdate(ctx, domain.UpdateJob{UpdateID: staleID, ActorID: staleID, Payload: []byte("encrypted")}); err != nil || !inserted {
			t.Fatalf("enqueue stale lease job = %v, %v", inserted, err)
		}
		stale, err := postgres.ClaimUpdate(ctx, "stale-lease", now, time.Minute)
		if err != nil || stale.UpdateID != staleID {
			t.Fatalf("stale first claim = %+v, %v", stale, err)
		}
		recovered, err := postgres.ClaimUpdate(ctx, "recovered-lease", now.Add(time.Minute), time.Minute)
		if err != nil || recovered.UpdateID != staleID || recovered.Attempts != 2 {
			t.Fatalf("stale recovered claim = %+v, %v", recovered, err)
		}
		if err := postgres.CompleteUpdate(ctx, staleID, stale.LeaseToken, now); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("stale lease completion = %v", err)
		}
		if err := postgres.CompleteUpdate(ctx, staleID, recovered.LeaseToken, now); err != nil {
			t.Fatalf("recovered lease completion = %v", err)
		}
	})

	t.Run("supersession scrubs generations but preserves commands", func(t *testing.T) {
		const actorID int64 = 71750
		for _, job := range []domain.UpdateJob{
			{UpdateID: 71751, ActorID: actorID, Payload: []byte("old-generation"), Supersedable: true},
			{UpdateID: 71752, ActorID: actorID, Payload: []byte("help-command")},
		} {
			if inserted, err := postgres.EnqueueUpdate(ctx, job); err != nil || !inserted {
				t.Fatalf("enqueue %d = %v, %v", job.UpdateID, inserted, err)
			}
		}
		old, err := postgres.ClaimUpdate(ctx, "superseded-lease", now, time.Minute)
		if err != nil || old.UpdateID != 71751 {
			t.Fatalf("claim old generation = %+v, %v", old, err)
		}
		if inserted, err := postgres.EnqueueUpdate(ctx, domain.UpdateJob{
			UpdateID: 71753, ActorID: actorID, Payload: []byte("replacement"), Supersedable: true, Superseding: true,
		}); err != nil || !inserted {
			t.Fatalf("enqueue replacement = %v, %v", inserted, err)
		}
		if err := postgres.RenewUpdate(ctx, old.UpdateID, old.LeaseToken, now, time.Minute); !errors.Is(err, ErrSuperseded) {
			t.Fatalf("superseded renewal = %v", err)
		}
		var status domain.UpdateJobStatus
		var payloadLength int
		if err := postgres.pool.QueryRow(ctx, `
			SELECT status, octet_length(payload) FROM telegram_update_jobs WHERE update_id = $1`, old.UpdateID,
		).Scan(&status, &payloadLength); err != nil {
			t.Fatal(err)
		}
		if status != domain.UpdateJobSuperseded || payloadLength != 0 {
			t.Fatalf("old row = status %q payload %d", status, payloadLength)
		}
		command, err := postgres.ClaimUpdate(ctx, "preserved-command", now, time.Minute)
		if err != nil || command.UpdateID != 71752 {
			t.Fatalf("non-generative command was skipped: %+v, %v", command, err)
		}
		if err := postgres.CompleteUpdate(ctx, command.UpdateID, command.LeaseToken, now); err != nil {
			t.Fatal(err)
		}
		replacement, err := postgres.ClaimUpdate(ctx, "replacement-lease", now, time.Minute)
		if err != nil || replacement.UpdateID != 71753 || !replacement.Supersedable || !replacement.Superseding {
			t.Fatalf("replacement claim = %+v, %v", replacement, err)
		}
		if err := postgres.CompleteUpdate(ctx, replacement.UpdateID, replacement.LeaseToken, now); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("profile deletion cascades", func(t *testing.T) {
		if err := postgres.SetConsent(ctx, ownerID, true); err != nil {
			t.Fatalf("SetConsent(): %v", err)
		}
		if err := postgres.RecordFeedback(ctx, domain.Feedback{GenerationID: generationID, TelegramID: ownerID, Candidate: 0, Rating: 1, Action: "rating"}); err != nil {
			t.Fatalf("RecordFeedback(): %v", err)
		}
		if saved, err := postgres.SaveStyleExample(ctx, ownerID, generationID, "Сохранённый стиль", 5); err != nil || !saved {
			t.Fatalf("SaveStyleExample(): %v", err)
		}
		if _, err := postgres.pool.Exec(ctx, `INSERT INTO entitlements (telegram_id, plan) VALUES ($1, 'test')`, ownerID); err != nil {
			t.Fatalf("insert entitlement: %v", err)
		}
		if inserted, err := postgres.EnqueueUpdate(ctx, domain.UpdateJob{UpdateID: 71801, ActorID: ownerID, Payload: []byte("encrypted")}); err != nil || !inserted {
			t.Fatalf("EnqueueUpdate(): %v, %v", inserted, err)
		}
		if err := postgres.DeleteUser(ctx, ownerID); err != nil {
			t.Fatalf("DeleteUser(): %v", err)
		}
		if _, err := postgres.GetUser(ctx, ownerID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetUser() after deletion error = %v", err)
		}
		for _, table := range []string{"users", "entitlements", "daily_usage", "quota_reservations", "generations", "thread_drafts", "feedback", "style_examples"} {
			var count int
			query := fmt.Sprintf("SELECT count(*) FROM %s WHERE telegram_id = $1", pgx.Identifier{table}.Sanitize())
			if err := postgres.pool.QueryRow(ctx, query, ownerID).Scan(&count); err != nil {
				t.Fatalf("count %s: %v", table, err)
			}
			if count != 0 {
				t.Errorf("%s retained %d deleted-user rows", table, count)
			}
		}
		var queued int
		if err := postgres.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_update_jobs WHERE actor_id = $1`, ownerID).Scan(&queued); err != nil {
			t.Fatalf("count telegram_update_jobs: %v", err)
		}
		if queued != 0 {
			t.Fatalf("telegram_update_jobs retained %d deleted-user rows", queued)
		}
	})

	t.Run("retention removes only expired durable content", func(t *testing.T) {
		const retentionUserID = int64(71005)
		if _, err := postgres.UpsertUser(ctx, domain.User{TelegramID: retentionUserID, Language: "kk", DefaultTone: domain.ToneMix}); err != nil {
			t.Fatalf("UpsertUser(): %v", err)
		}
		oldGenerationID, err := postgres.SaveGeneration(ctx, integrationGeneration(retentionUserID, "old-digest"))
		if err != nil {
			t.Fatalf("save old generation: %v", err)
		}
		newGenerationID, err := postgres.SaveGeneration(ctx, integrationGeneration(retentionUserID, "new-digest"))
		if err != nil {
			t.Fatalf("save new generation: %v", err)
		}
		oldDraftID, err := postgres.CreateThreadDraft(ctx, testThreadDraft(retentionUserID, 1, "Старый Threads-пост"))
		if err != nil {
			t.Fatalf("save old thread draft: %v", err)
		}
		newDraftID, err := postgres.CreateThreadDraft(ctx, testThreadDraft(retentionUserID, 2, "Новый Threads-пост"))
		if err != nil {
			t.Fatalf("save new thread draft: %v", err)
		}
		if err := postgres.RecordFeedback(ctx, domain.Feedback{GenerationID: oldGenerationID, TelegramID: retentionUserID, Candidate: 0, Rating: 1, Action: "rating"}); err != nil {
			t.Fatalf("record old feedback: %v", err)
		}
		oldTime := now.Add(-48 * time.Hour)
		if _, err := postgres.pool.Exec(ctx, `UPDATE generations SET created_at = $2 WHERE id = $1`, oldGenerationID, oldTime); err != nil {
			t.Fatalf("age generation: %v", err)
		}
		if _, err := postgres.pool.Exec(ctx, `UPDATE thread_drafts SET updated_at = $2 WHERE id = $1`, oldDraftID, oldTime); err != nil {
			t.Fatalf("age thread draft: %v", err)
		}
		const oldUpdateID, newUpdateID = int64(71901), int64(71902)
		for _, updateID := range []int64{oldUpdateID, newUpdateID} {
			decision, err := postgres.ConsumeQuota(ctx, retentionUserID, updateID, domain.QuotaText, 10, now)
			if err != nil || !decision.Allowed {
				t.Fatalf("ConsumeQuota(%d) = %+v, %v", updateID, decision, err)
			}
			inserted, err := postgres.EnqueueUpdate(ctx, domain.UpdateJob{UpdateID: updateID, ActorID: retentionUserID, Payload: []byte("encrypted")})
			if err != nil || !inserted {
				t.Fatalf("EnqueueUpdate(%d) = %v, %v", updateID, inserted, err)
			}
			job, err := postgres.ClaimUpdate(ctx, fmt.Sprintf("lease-%d", updateID), now, time.Minute)
			if err != nil || job.UpdateID != updateID {
				t.Fatalf("ClaimUpdate(%d) = %+v, %v", updateID, job, err)
			}
			if err := postgres.CompleteUpdate(ctx, job.UpdateID, job.LeaseToken, now); err != nil {
				t.Fatalf("CompleteUpdate(%d): %v", updateID, err)
			}
		}
		if _, err := postgres.pool.Exec(ctx, `UPDATE telegram_update_jobs SET completed_at = $2 WHERE update_id = $1`, oldUpdateID, oldTime); err != nil {
			t.Fatalf("age update: %v", err)
		}
		if _, err := postgres.pool.Exec(ctx, `UPDATE quota_reservations SET updated_at = $2 WHERE telegram_id = $1 AND reservation_id = $3`, retentionUserID, oldTime, oldUpdateID); err != nil {
			t.Fatalf("age quota reservation: %v", err)
		}

		deleted, err := postgres.Cleanup(ctx, now.Add(-24*time.Hour))
		if err != nil {
			t.Fatalf("Cleanup(): %v", err)
		}
		if deleted != 1 {
			t.Fatalf("Cleanup() deleted = %d, want 1 generation", deleted)
		}
		if _, err := postgres.GetGeneration(ctx, oldGenerationID, retentionUserID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("old GetGeneration() error = %v", err)
		}
		if _, err := postgres.GetGeneration(ctx, newGenerationID, retentionUserID); err != nil {
			t.Fatalf("new GetGeneration(): %v", err)
		}
		if _, err := postgres.GetThreadDraft(ctx, oldDraftID, retentionUserID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("old GetThreadDraft() error = %v", err)
		}
		if _, err := postgres.GetThreadDraft(ctx, newDraftID, retentionUserID); err != nil {
			t.Fatalf("new GetThreadDraft(): %v", err)
		}
		if inserted, err := postgres.EnqueueUpdate(ctx, domain.UpdateJob{UpdateID: oldUpdateID, ActorID: retentionUserID, Payload: []byte("encrypted")}); err != nil || !inserted {
			t.Fatalf("expired update was not released: inserted=%v err=%v", inserted, err)
		}
		if inserted, err := postgres.EnqueueUpdate(ctx, domain.UpdateJob{UpdateID: newUpdateID, ActorID: retentionUserID, Payload: []byte("encrypted")}); err != nil || inserted {
			t.Fatalf("fresh update lost deduplication: inserted=%v err=%v", inserted, err)
		}
		for _, check := range []struct {
			updateID int64
			want     int
		}{
			{updateID: oldUpdateID, want: 0},
			{updateID: newUpdateID, want: 1},
		} {
			var count int
			if err := postgres.pool.QueryRow(ctx, `SELECT count(*) FROM quota_reservations WHERE telegram_id = $1 AND reservation_id = $2`, retentionUserID, check.updateID).Scan(&count); err != nil {
				t.Fatalf("count quota reservation %d: %v", check.updateID, err)
			}
			if count != check.want {
				t.Fatalf("quota reservation %d count = %d, want %d", check.updateID, count, check.want)
			}
		}
		var feedbackCount int
		if err := postgres.pool.QueryRow(ctx, `SELECT count(*) FROM feedback WHERE generation_id = $1`, oldGenerationID).Scan(&feedbackCount); err != nil {
			t.Fatalf("count feedback: %v", err)
		}
		if feedbackCount != 0 {
			t.Fatalf("expired feedback rows = %d", feedbackCount)
		}
	})
}

func TestPostgresMediaMigrationUpgradesLegacyThreadDrafts(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	postgres := newIsolatedPostgresWithoutMigration(t, ctx, databaseURL)

	// This is the deployed pre-media shape at the parent commit. Keeping a
	// representative row proves that the additive migration backfills existing
	// drafts instead of validating only a freshly created schema.
	if _, err := postgres.pool.Exec(ctx, `
		CREATE TABLE users (
			telegram_id BIGINT PRIMARY KEY,
			language_code TEXT NOT NULL DEFAULT 'ru',
			default_tone TEXT NOT NULL DEFAULT 'mix',
			consented_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE TABLE thread_drafts (
			id BIGSERIAL PRIMARY KEY,
			telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
			voice TEXT NOT NULL CHECK (voice IN ('belcanto', 'alisher')),
			goal TEXT NOT NULL,
			preview_text TEXT NOT NULL,
			provider TEXT NOT NULL,
			model TEXT NOT NULL,
			revision BIGINT NOT NULL,
			state TEXT NOT NULL DEFAULT 'draft',
			is_current BOOLEAN NOT NULL DEFAULT TRUE,
			container_id TEXT NOT NULL DEFAULT '',
			post_id TEXT NOT NULL DEFAULT '',
			permalink TEXT NOT NULL DEFAULT '',
			error_code TEXT NOT NULL DEFAULT '',
			claim_token TEXT NOT NULL DEFAULT '',
			claim_expires_at TIMESTAMPTZ,
			publish_started_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			published_at TIMESTAMPTZ
		);
		INSERT INTO users (telegram_id) VALUES (72001);
		INSERT INTO thread_drafts (
			telegram_id, voice, goal, preview_text, provider, model, revision
		) VALUES (72001, 'belcanto', 'discussion', 'Старый текстовый пост', 'legacy', 'legacy', 7);`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := postgres.Migrate(ctx); err != nil {
		t.Fatalf("upgrade legacy schema: %v", err)
	}
	if err := postgres.Migrate(ctx); err != nil {
		t.Fatalf("rerun upgraded migration: %v", err)
	}
	legacy, err := postgres.GetCurrentThreadDraft(ctx, 72001)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.MediaMode != domain.ThreadMediaText || legacy.MediaID != 0 || legacy.MediaRightsConfirmedAt != nil {
		t.Fatalf("legacy media backfill = %+v", legacy)
	}
	pending, err := postgres.SetThreadDraftMediaMode(ctx, legacy.ID, 72001, legacy.Revision, domain.ThreadMediaImagePending)
	if err != nil {
		t.Fatal(err)
	}
	attached, err := postgres.AttachThreadDraftMedia(ctx, legacy.ID, 72001, pending.Revision, testThreadMedia(72001, 99001))
	if err != nil {
		t.Fatal(err)
	}
	claimedDraft, claimed, err := postgres.ClaimThreadDraft(
		ctx, legacy.ID, 72001, attached.Revision, "legacy-media-claim", time.Now().UTC(), time.Minute,
	)
	if err != nil || !claimed || claimedDraft.MediaRightsConfirmedAt == nil {
		t.Fatalf("upgraded image claim = %+v, %v, %v", claimedDraft, claimed, err)
	}
}

func newIsolatedPostgres(t *testing.T, ctx context.Context, databaseURL string) *Postgres {
	t.Helper()
	postgres := newIsolatedPostgresWithoutMigration(t, ctx, databaseURL)
	if err := postgres.Migrate(ctx); err != nil {
		t.Fatalf("Migrate(): %v", err)
	}
	return postgres
}

func newIsolatedPostgresWithoutMigration(t *testing.T, ctx context.Context, databaseURL string) *Postgres {
	t.Helper()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect integration database: %v", err)
	}
	schema := fmt.Sprintf("witty_it_%d", time.Now().UnixNano())
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		admin.Close()
		t.Fatalf("create integration schema: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		admin.Close()
		t.Fatalf("parse integration database URL: %v", err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
		t.Fatalf("open isolated integration pool: %v", err)
	}
	postgres := &Postgres{pool: pool}
	t.Cleanup(func() {
		pool.Close()
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanupCtx, "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
	})
	return postgres
}

func integrationGeneration(telegramID int64, digest string) domain.GenerationRecord {
	return domain.GenerationRecord{
		TelegramID:  telegramID,
		InputKind:   domain.InputText,
		InputDigest: digest,
		Tone:        domain.ToneMix,
		Provider:    "integration-test",
		Model:       "deterministic",
		Result: domain.GenerationResult{
			Mode: domain.ScenarioComment, ModeConfidence: domain.ModeConfidenceHigh,
			Situation: "Тест",
			Replies: []domain.Reply{
				{Tone: domain.ToneSmart, Text: "Первый"},
				{Tone: domain.TonePlayful, Text: "Второй"},
				{Tone: domain.ToneBoundary, Text: "Третий"},
			},
		},
	}
}
