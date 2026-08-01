package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aleka7sk/witty-reply/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type Postgres struct {
	pool *pgxpool.Pool
}

func NewPostgres(ctx context.Context, databaseURL string, migrate bool) (*Postgres, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database config: %w", err)
	}
	config.MaxConns = 10
	config.MinConns = 1
	config.MaxConnLifetime = 30 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	store := &Postgres{pool: pool}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if migrate {
		if err := store.Migrate(ctx); err != nil {
			pool.Close()
			return nil, err
		}
	}
	if err := store.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

func (p *Postgres) Migrate(ctx context.Context) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer rollback(tx)
	// Serialize startup migrations across replicas without holding a permanent
	// session lock. The transaction also makes each version all-or-nothing.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8793542106471132)`); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	var hasMigrationTable bool
	if err := tx.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&hasMigrationTable); err != nil {
		return fmt.Errorf("inspect migrations: %w", err)
	}
	if hasMigrationTable {
		var versionOneApplied, versionTwoApplied bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 1),
			       EXISTS (SELECT 1 FROM schema_migrations WHERE version = 2)`,
		).Scan(&versionOneApplied, &versionTwoApplied); err != nil {
			return fmt.Errorf("inspect legacy migration state: %w", err)
		}
		if versionOneApplied && !versionTwoApplied {
			// Version 1 was historically an idempotent mutable bootstrap. Early
			// installations marked it applied before Belcanto draft/media tables
			// existed. Run the final compatibility bootstrap exactly once on the
			// transition to version 2; later immutable migrations then run once.
			script, err := migrationFS.ReadFile("migrations/001_init.sql")
			if err != nil {
				return fmt.Errorf("read legacy compatibility bootstrap: %w", err)
			}
			if _, err := tx.Exec(ctx, string(script)); err != nil {
				return fmt.Errorf("apply legacy compatibility bootstrap: %w", err)
			}
		}
	}
	for _, migration := range []struct {
		version int
		path    string
	}{
		{version: 1, path: "migrations/001_init.sql"},
		{version: 2, path: "migrations/002_licensed_thread_media.sql"},
		{version: 3, path: "migrations/003_manual_licensed_media.sql"},
		{version: 4, path: "migrations/004_thread_briefs.sql"},
	} {
		applied := false
		if hasMigrationTable {
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, migration.version).Scan(&applied); err != nil {
				return fmt.Errorf("check migration %d: %w", migration.version, err)
			}
		}
		if applied {
			continue
		}
		script, err := migrationFS.ReadFile(migration.path)
		if err != nil {
			return fmt.Errorf("read migration %d: %w", migration.version, err)
		}
		if _, err := tx.Exec(ctx, string(script)); err != nil {
			return fmt.Errorf("apply migration %d: %w", migration.version, err)
		}
		hasMigrationTable = true
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}

func (p *Postgres) Ping(ctx context.Context) error {
	if err := p.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	var ready bool
	if err := p.pool.QueryRow(ctx, `
		SELECT to_regclass('users') IS NOT NULL
		   AND to_regclass('telegram_update_jobs') IS NOT NULL
		   AND to_regclass('quota_reservations') IS NOT NULL
		   AND to_regclass('generations') IS NOT NULL
		   AND to_regclass('thread_media') IS NOT NULL
		   AND to_regclass('thread_media_attach_operations') IS NOT NULL
		   AND to_regclass('thread_briefs') IS NOT NULL
		   AND to_regclass('thread_drafts') IS NOT NULL
		   AND EXISTS (SELECT 1 FROM schema_migrations WHERE version = 4)`).Scan(&ready); err != nil {
		return fmt.Errorf("check database schema: %w", err)
	}
	if !ready {
		return errors.New("database schema is not ready; run migrations")
	}
	return nil
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) EnqueueUpdate(ctx context.Context, job domain.UpdateJob) (bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	command, err := tx.Exec(ctx, `
		INSERT INTO telegram_update_jobs (update_id, actor_id, payload, supersedable, superseding)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (update_id) DO NOTHING`,
		job.UpdateID, job.ActorID, job.Payload, job.Supersedable, job.Superseding)
	if err != nil {
		return false, err
	}
	inserted := command.RowsAffected() == 1
	if inserted && job.Superseding {
		if _, err := tx.Exec(ctx, `
			UPDATE telegram_update_jobs
			SET status = 'superseded', payload = ''::bytea, available_at = now(),
				lease_token = NULL, lease_until = NULL, last_error = 'superseded', completed_at = now()
			WHERE actor_id = $1 AND update_id < $2 AND supersedable
			  AND status IN ('pending', 'processing')`, job.ActorID, job.UpdateID); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return inserted, nil
}

func (p *Postgres) ClaimUpdate(ctx context.Context, leaseToken string, now time.Time, lease time.Duration) (domain.UpdateJob, error) {
	var job domain.UpdateJob
	err := p.pool.QueryRow(ctx, `
		WITH candidate AS (
			SELECT queued.update_id
			FROM telegram_update_jobs AS queued
			WHERE (
				(queued.status = 'pending' AND queued.available_at <= $2)
				OR (queued.status = 'processing' AND queued.lease_until <= $2)
			)
			AND NOT EXISTS (
				SELECT 1
				FROM telegram_update_jobs AS earlier
				WHERE earlier.actor_id = queued.actor_id
				  AND earlier.status IN ('pending', 'processing')
				  AND earlier.update_id < queued.update_id
			)
			ORDER BY queued.update_id
			FOR UPDATE OF queued SKIP LOCKED
			LIMIT 1
		)
		UPDATE telegram_update_jobs AS claimed
		SET status = 'processing', attempts = claimed.attempts + 1,
			lease_token = $1, lease_until = $3
		FROM candidate
		WHERE claimed.update_id = candidate.update_id
		RETURNING claimed.update_id, claimed.actor_id, claimed.payload,
			claimed.supersedable, claimed.superseding, claimed.status,
			claimed.attempts, claimed.available_at, claimed.lease_until,
			claimed.lease_token, claimed.enqueued_at`,
		leaseToken, now, now.Add(lease),
	).Scan(
		&job.UpdateID, &job.ActorID, &job.Payload,
		&job.Supersedable, &job.Superseding, &job.Status,
		&job.Attempts, &job.AvailableAt, &job.LeaseUntil,
		&job.LeaseToken, &job.EnqueuedAt,
	)
	return job, mapNotFound(err)
}

func (p *Postgres) RenewUpdate(ctx context.Context, updateID int64, leaseToken string, now time.Time, lease time.Duration) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE telegram_update_jobs
		SET lease_until = $4
		WHERE update_id = $1 AND status = 'processing' AND lease_token = $2
		  AND lease_until > $3`,
		updateID, leaseToken, now, now.Add(lease))
	if err != nil || tag.RowsAffected() != 0 {
		return err
	}
	return p.mapQueueMutationError(ctx, updateID)
}

func (p *Postgres) CompleteUpdate(ctx context.Context, updateID int64, leaseToken string, now time.Time) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE telegram_update_jobs
		SET status = 'completed', payload = ''::bytea, available_at = $3,
			lease_token = NULL, lease_until = NULL, last_error = '', completed_at = $3
		WHERE update_id = $1 AND status = 'processing' AND lease_token = $2`,
		updateID, leaseToken, now)
	if err != nil || tag.RowsAffected() != 0 {
		return err
	}
	return p.mapQueueMutationError(ctx, updateID)
}

func (p *Postgres) RetryUpdate(ctx context.Context, updateID int64, leaseToken string, now, retryAt time.Time, errorCode string, maxAttempts int) (bool, error) {
	var status domain.UpdateJobStatus
	err := p.pool.QueryRow(ctx, `
		UPDATE telegram_update_jobs
		SET status = CASE WHEN attempts >= $6 THEN 'dead' ELSE 'pending' END,
			payload = CASE WHEN attempts >= $6 THEN ''::bytea ELSE payload END,
			available_at = CASE WHEN attempts >= $6 THEN $3::timestamptz ELSE $4::timestamptz END,
			lease_token = NULL, lease_until = NULL, last_error = $5,
			completed_at = CASE WHEN attempts >= $6 THEN $3::timestamptz ELSE NULL END
		WHERE update_id = $1 AND status = 'processing' AND lease_token = $2
		RETURNING status`,
		updateID, leaseToken, now, retryAt, truncateErrorCode(errorCode), maxAttempts,
	).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, p.mapQueueMutationError(ctx, updateID)
	}
	return status == domain.UpdateJobDead, err
}

func (p *Postgres) QueueStats(ctx context.Context, now time.Time) (int64, time.Duration, error) {
	var pending int64
	var oldestSeconds float64
	err := p.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(EXTRACT(EPOCH FROM ($1 - min(enqueued_at))), 0)
		FROM telegram_update_jobs WHERE status IN ('pending', 'processing')`, now,
	).Scan(&pending, &oldestSeconds)
	if err != nil {
		return 0, 0, err
	}
	if oldestSeconds < 0 {
		oldestSeconds = 0
	}
	return pending, time.Duration(oldestSeconds * float64(time.Second)), nil
}

func (p *Postgres) mapQueueMutationError(ctx context.Context, updateID int64) error {
	var status domain.UpdateJobStatus
	err := p.pool.QueryRow(ctx, `SELECT status FROM telegram_update_jobs WHERE update_id = $1`, updateID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		// /delete_me removes this actor's pending and in-flight jobs. Finishing
		// that command remains an idempotent success.
		return nil
	}
	if err != nil {
		return err
	}
	if status == domain.UpdateJobSuperseded {
		return ErrSuperseded
	}
	return ErrLeaseLost
}

func (p *Postgres) UpsertUser(ctx context.Context, user domain.User) (domain.User, error) {
	err := p.pool.QueryRow(ctx, `
		INSERT INTO users (telegram_id, language_code, default_tone)
		VALUES ($1, $2, $3)
		ON CONFLICT (telegram_id) DO UPDATE SET
			language_code = CASE WHEN EXCLUDED.language_code = '' THEN users.language_code ELSE EXCLUDED.language_code END,
			updated_at = now()
		RETURNING telegram_id, language_code, default_tone, consented_at, created_at, updated_at`,
		user.TelegramID, fallback(user.Language, "ru"), fallback(string(user.DefaultTone), string(domain.ToneMix)),
	).Scan(&user.TelegramID, &user.Language, &user.DefaultTone, &user.ConsentedAt, &user.CreatedAt, &user.UpdatedAt)
	return user, err
}

func (p *Postgres) GetUser(ctx context.Context, telegramID int64) (domain.User, error) {
	var user domain.User
	err := p.pool.QueryRow(ctx, `
		SELECT telegram_id, language_code, default_tone, consented_at, created_at, updated_at
		FROM users WHERE telegram_id = $1`, telegramID,
	).Scan(&user.TelegramID, &user.Language, &user.DefaultTone, &user.ConsentedAt, &user.CreatedAt, &user.UpdatedAt)
	return user, mapNotFound(err)
}

func (p *Postgres) SetConsent(ctx context.Context, telegramID int64, consent bool) error {
	value := any(nil)
	if consent {
		value = time.Now().UTC()
	}
	tag, err := p.pool.Exec(ctx, `UPDATE users SET consented_at = $2, updated_at = now() WHERE telegram_id = $1`, telegramID, value)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (p *Postgres) SetDefaultTone(ctx context.Context, telegramID int64, tone domain.Tone) error {
	tag, err := p.pool.Exec(ctx, `UPDATE users SET default_tone = $2, updated_at = now() WHERE telegram_id = $1`, telegramID, tone)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (p *Postgres) ConsumeQuota(ctx context.Context, telegramID, reservationID int64, category domain.QuotaCategory, defaultLimit int, now time.Time) (domain.QuotaDecision, error) {
	if reservationID <= 0 || category == "" || defaultLimit < 1 {
		return domain.QuotaDecision{}, errors.New("invalid quota reservation")
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.QuotaDecision{}, err
	}
	defer rollback(tx)

	var lockedID int64
	if err := tx.QueryRow(ctx, `SELECT telegram_id FROM users WHERE telegram_id = $1 FOR UPDATE`, telegramID).Scan(&lockedID); err != nil {
		return domain.QuotaDecision{}, mapNotFound(err)
	}
	existing, found, err := loadQuotaReservation(ctx, tx, telegramID, reservationID, category)
	if err != nil {
		return domain.QuotaDecision{}, err
	}
	if found {
		switch existing.state {
		case "charged", "denied":
			existing.decision.ResetsAt = existing.decision.ResetsAt.In(now.Location())
			if err := tx.Commit(ctx); err != nil {
				return domain.QuotaDecision{}, err
			}
			return existing.decision, nil
		case "refunded":
			// A failed delivery released the slot. The durable retry gets one
			// atomic chance to reserve the current quota window again.
		default:
			return domain.QuotaDecision{}, fmt.Errorf("invalid quota reservation state %q", existing.state)
		}
	}

	plan, limit, err := quotaPlan(ctx, tx, telegramID, category, defaultLimit, now)
	if err != nil {
		return domain.QuotaDecision{}, err
	}
	day := now.Format("2006-01-02")
	used, allowed, err := reserveDailyUsage(ctx, tx, telegramID, category, day, limit)
	if err != nil {
		return domain.QuotaDecision{}, err
	}
	decision := domain.QuotaDecision{Allowed: allowed, Used: used, Limit: limit, ResetsAt: nextDay(now), Plan: plan}
	state := "denied"
	if allowed {
		state = "charged"
	}
	if found {
		_, err = tx.Exec(ctx, `
			UPDATE quota_reservations
			SET state = $4, usage_day = $5::date, used_snapshot = $6,
				limit_snapshot = $7, resets_at = $8, plan_snapshot = $9, updated_at = now()
			WHERE telegram_id = $1 AND reservation_id = $2 AND category = $3`,
			telegramID, reservationID, category, state, day, used, limit, decision.ResetsAt, plan)
	} else {
		_, err = tx.Exec(ctx, `
			INSERT INTO quota_reservations (
				telegram_id, reservation_id, category, state, usage_day,
				used_snapshot, limit_snapshot, resets_at, plan_snapshot
			) VALUES ($1, $2, $3, $4, $5::date, $6, $7, $8, $9)`,
			telegramID, reservationID, category, state, day, used, limit, decision.ResetsAt, plan)
	}
	if err != nil {
		return domain.QuotaDecision{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.QuotaDecision{}, err
	}
	return decision, nil
}

func (p *Postgres) RefundQuota(ctx context.Context, telegramID, reservationID int64, category domain.QuotaCategory) error {
	if reservationID <= 0 || category == "" {
		return errors.New("invalid quota reservation")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)

	var lockedID int64
	if err := tx.QueryRow(ctx, `SELECT telegram_id FROM users WHERE telegram_id = $1 FOR UPDATE`, telegramID).Scan(&lockedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	var state, day string
	err = tx.QueryRow(ctx, `
		SELECT state, usage_day::text
		FROM quota_reservations
		WHERE telegram_id = $1 AND reservation_id = $2 AND category = $3
		FOR UPDATE`, telegramID, reservationID, category).Scan(&state, &day)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	if state == "refunded" || state == "denied" {
		return tx.Commit(ctx)
	}
	if state != "charged" {
		return fmt.Errorf("invalid quota reservation state %q", state)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE daily_usage SET used = used - 1
		WHERE telegram_id = $1 AND usage_day = $2::date AND category = $3 AND used > 0`,
		telegramID, day, category)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("charged quota reservation has no usage slot")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE quota_reservations SET state = 'refunded', updated_at = now()
		WHERE telegram_id = $1 AND reservation_id = $2 AND category = $3`,
		telegramID, reservationID, category); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type quotaReservationRecord struct {
	state    string
	decision domain.QuotaDecision
}

func loadQuotaReservation(ctx context.Context, tx pgx.Tx, telegramID, reservationID int64, category domain.QuotaCategory) (quotaReservationRecord, bool, error) {
	var record quotaReservationRecord
	err := tx.QueryRow(ctx, `
		SELECT state, used_snapshot, limit_snapshot, resets_at, plan_snapshot
		FROM quota_reservations
		WHERE telegram_id = $1 AND reservation_id = $2 AND category = $3
		FOR UPDATE`, telegramID, reservationID, category).Scan(
		&record.state, &record.decision.Used, &record.decision.Limit,
		&record.decision.ResetsAt, &record.decision.Plan,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return quotaReservationRecord{}, false, nil
	}
	if err != nil {
		return quotaReservationRecord{}, false, err
	}
	record.decision.Allowed = record.state == "charged"
	return record, true, nil
}

func quotaPlan(ctx context.Context, tx pgx.Tx, telegramID int64, category domain.QuotaCategory, defaultLimit int, now time.Time) (string, int, error) {
	plan, limit := "free", defaultLimit
	var limitsJSON []byte
	err := tx.QueryRow(ctx, `
		SELECT plan, limits FROM entitlements
		WHERE telegram_id = $1 AND (expires_at IS NULL OR expires_at > $2)`, telegramID, now).Scan(&plan, &limitsJSON)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", 0, err
	}
	if err == nil {
		var limits map[string]int
		if json.Unmarshal(limitsJSON, &limits) == nil {
			if custom := limits[string(category)]; custom > 0 {
				limit = custom
			}
		}
	}
	return plan, limit, nil
}

func reserveDailyUsage(ctx context.Context, tx pgx.Tx, telegramID int64, category domain.QuotaCategory, day string, limit int) (int, bool, error) {
	var used int
	err := tx.QueryRow(ctx, `
		INSERT INTO daily_usage (telegram_id, usage_day, category, used)
		VALUES ($1, $2::date, $3, 1)
		ON CONFLICT (telegram_id, usage_day, category) DO UPDATE
		SET used = daily_usage.used + 1
		WHERE daily_usage.used < $4
		RETURNING used`, telegramID, day, category, limit).Scan(&used)
	allowed := true
	if errors.Is(err, pgx.ErrNoRows) {
		allowed = false
		err = tx.QueryRow(ctx, `SELECT used FROM daily_usage WHERE telegram_id = $1 AND usage_day = $2::date AND category = $3`, telegramID, day, category).Scan(&used)
	}
	if err != nil {
		return 0, false, err
	}
	return used, allowed, nil
}

func (p *Postgres) SaveGeneration(ctx context.Context, record domain.GenerationRecord) (int64, error) {
	result, err := json.Marshal(record.Result)
	if err != nil {
		return 0, err
	}
	var id int64
	err = p.pool.QueryRow(ctx, `
		INSERT INTO generations (telegram_id, input_kind, input_digest, tone, provider, model, result)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`, record.TelegramID, record.InputKind, record.InputDigest, record.Tone, record.Provider, record.Model, result).Scan(&id)
	return id, err
}

func (p *Postgres) GetGeneration(ctx context.Context, id, telegramID int64) (domain.GenerationRecord, error) {
	var record domain.GenerationRecord
	var result []byte
	err := p.pool.QueryRow(ctx, `
		SELECT id, telegram_id, input_kind, input_digest, tone, provider, model, result, created_at
		FROM generations WHERE id = $1 AND telegram_id = $2`, id, telegramID,
	).Scan(&record.ID, &record.TelegramID, &record.InputKind, &record.InputDigest, &record.Tone, &record.Provider, &record.Model, &result, &record.CreatedAt)
	if err != nil {
		return record, mapNotFound(err)
	}
	if err := json.Unmarshal(result, &record.Result); err != nil {
		return record, err
	}
	return record, nil
}

func (p *Postgres) StartThreadBrief(
	ctx context.Context,
	telegramID, startUpdateID int64,
	voice domain.ThreadVoice,
) (domain.ThreadBrief, bool, error) {
	brief := domain.ThreadBrief{TelegramID: telegramID, StartUpdateID: startUpdateID, Voice: voice}
	if err := brief.ValidateForStart(); err != nil {
		return domain.ThreadBrief{}, false, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return domain.ThreadBrief{}, false, err
	}
	defer rollback(tx)
	var owner int64
	if err := tx.QueryRow(ctx, `SELECT telegram_id FROM users WHERE telegram_id = $1 FOR UPDATE`, brief.TelegramID).Scan(&owner); err != nil {
		return domain.ThreadBrief{}, false, mapNotFound(err)
	}
	existing, err := scanThreadBrief(tx.QueryRow(ctx, threadBriefSelect+`
		WHERE telegram_id = $1 AND start_update_id = $2`, brief.TelegramID, brief.StartUpdateID))
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return domain.ThreadBrief{}, false, err
		}
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.ThreadBrief{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE thread_briefs SET is_current = FALSE, updated_at = now()
		WHERE telegram_id = $1 AND is_current`, brief.TelegramID); err != nil {
		return domain.ThreadBrief{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE thread_drafts SET is_current = FALSE, updated_at = now()
		WHERE telegram_id = $1 AND is_current`, brief.TelegramID); err != nil {
		return domain.ThreadBrief{}, false, err
	}
	created, err := scanThreadBrief(tx.QueryRow(ctx, `
		INSERT INTO thread_briefs (
			telegram_id, start_update_id, voice, objective, material_kind,
			material_text, material_update_id, state, revision, is_current, error_code
		) VALUES ($1, $2, $3, '', '', '', NULL, 'awaiting_goal', 1, TRUE, '')
		RETURNING `+threadBriefColumns,
		brief.TelegramID, brief.StartUpdateID, brief.Voice,
	))
	if err != nil {
		return domain.ThreadBrief{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ThreadBrief{}, false, err
	}
	return created, true, nil
}

func (p *Postgres) GetThreadBrief(ctx context.Context, id, telegramID int64) (domain.ThreadBrief, error) {
	brief, err := scanThreadBrief(p.pool.QueryRow(ctx, threadBriefSelect+`
		WHERE id = $1 AND telegram_id = $2`, id, telegramID))
	if err != nil {
		return domain.ThreadBrief{}, mapNotFound(err)
	}
	return brief, nil
}

func (p *Postgres) GetCurrentThreadBrief(ctx context.Context, telegramID int64) (domain.ThreadBrief, error) {
	brief, err := scanThreadBrief(p.pool.QueryRow(ctx, threadBriefSelect+`
		WHERE telegram_id = $1 AND is_current`, telegramID))
	if err != nil {
		return domain.ThreadBrief{}, mapNotFound(err)
	}
	return brief, nil
}

func (p *Postgres) SetThreadBriefObjective(
	ctx context.Context,
	id, telegramID int64,
	revision uint32,
	objective domain.ThreadObjective,
) (domain.ThreadBrief, error) {
	if !objective.Selectable() || revision == ^uint32(0) {
		return domain.ThreadBrief{}, ErrThreadBriefState
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return domain.ThreadBrief{}, err
	}
	defer rollback(tx)
	brief, err := scanThreadBrief(tx.QueryRow(ctx, `
		UPDATE thread_briefs
		SET objective = $4, state = 'awaiting_material', revision = revision + 1,
			error_code = '', updated_at = now()
		WHERE id = $1 AND telegram_id = $2 AND revision = $3 AND is_current
		  AND state IN ('awaiting_goal', 'awaiting_material')
		RETURNING `+threadBriefColumns,
		id, telegramID, revision, objective,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		existing, lookupErr := scanThreadBrief(tx.QueryRow(ctx, threadBriefSelect+`
			WHERE id = $1 AND telegram_id = $2`, id, telegramID))
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			return domain.ThreadBrief{}, ErrNotFound
		}
		if lookupErr != nil {
			return domain.ThreadBrief{}, lookupErr
		}
		if existing.Current && existing.Revision == revision+1 &&
			existing.State == domain.ThreadBriefAwaitingMaterial && existing.Objective == objective {
			if err := tx.Commit(ctx); err != nil {
				return domain.ThreadBrief{}, err
			}
			return existing, nil
		}
		return domain.ThreadBrief{}, ErrThreadBriefState
	}
	if err != nil {
		return domain.ThreadBrief{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ThreadBrief{}, err
	}
	return brief, nil
}

func (p *Postgres) SetThreadBriefMaterial(
	ctx context.Context,
	id, telegramID int64,
	revision uint32,
	updateID int64,
	kind domain.ThreadMaterialKind,
	value string,
) (domain.ThreadBrief, error) {
	if err := validateThreadMaterialChoice(updateID, kind, value); err != nil {
		return domain.ThreadBrief{}, err
	}
	if revision == ^uint32(0) {
		return domain.ThreadBrief{}, ErrThreadBriefState
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return domain.ThreadBrief{}, err
	}
	defer rollback(tx)
	existing, lookupErr := scanThreadBrief(tx.QueryRow(ctx, threadBriefSelect+`
		WHERE telegram_id = $1 AND material_update_id = $2`, telegramID, updateID))
	if lookupErr == nil {
		if existing.ID != id || existing.MaterialKind != kind || existing.MaterialText != value {
			return domain.ThreadBrief{}, ErrThreadBriefState
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.ThreadBrief{}, err
		}
		return existing, nil
	}
	if !errors.Is(lookupErr, pgx.ErrNoRows) {
		return domain.ThreadBrief{}, lookupErr
	}
	brief, err := scanThreadBrief(tx.QueryRow(ctx, `
		UPDATE thread_briefs
		SET material_kind = $4, material_text = $5, material_update_id = $6,
			state = 'material_ready', revision = revision + 1,
			error_code = '', updated_at = now()
		WHERE id = $1 AND telegram_id = $2 AND revision = $3 AND is_current
		  AND state = 'awaiting_material'
		RETURNING `+threadBriefColumns,
		id, telegramID, revision, kind, value, updateID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		replayed, replayErr := scanThreadBrief(tx.QueryRow(ctx, threadBriefSelect+`
			WHERE telegram_id = $1 AND material_update_id = $2`, telegramID, updateID))
		if replayErr == nil {
			if replayed.ID == id && replayed.MaterialKind == kind && replayed.MaterialText == value {
				if err := tx.Commit(ctx); err != nil {
					return domain.ThreadBrief{}, err
				}
				return replayed, nil
			}
			return domain.ThreadBrief{}, ErrThreadBriefState
		}
		if !errors.Is(replayErr, pgx.ErrNoRows) {
			return domain.ThreadBrief{}, replayErr
		}
		if _, lookupErr := scanThreadBrief(tx.QueryRow(ctx, threadBriefSelect+`
			WHERE id = $1 AND telegram_id = $2`, id, telegramID)); errors.Is(lookupErr, pgx.ErrNoRows) {
			return domain.ThreadBrief{}, ErrNotFound
		} else if lookupErr != nil {
			return domain.ThreadBrief{}, lookupErr
		}
		return domain.ThreadBrief{}, ErrThreadBriefState
	}
	if err != nil {
		return domain.ThreadBrief{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ThreadBrief{}, err
	}
	return brief, nil
}

func (p *Postgres) CreateThreadDraftForBrief(
	ctx context.Context,
	briefID int64,
	briefRevision uint32,
	draft domain.ThreadDraft,
	mediaInput *domain.ThreadMedia,
) (domain.ThreadDraft, bool, error) {
	if briefID <= 0 || draft.TelegramID <= 0 || draft.GenerationUpdateID <= 0 || briefRevision == ^uint32(0) {
		return domain.ThreadDraft{}, false, ErrThreadBriefState
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return domain.ThreadDraft{}, false, err
	}
	defer rollback(tx)
	// Use the same per-owner lock ordering as StartThreadBrief and the legacy
	// draft creators. Besides proving that the FK owner still exists, this
	// prevents a concurrent new brief (user -> brief) from deadlocking with
	// generation finalization (brief -> user through the draft/media FKs).
	var owner int64
	if err := tx.QueryRow(ctx, `SELECT telegram_id FROM users WHERE telegram_id = $1 FOR UPDATE`, draft.TelegramID).Scan(&owner); err != nil {
		return domain.ThreadDraft{}, false, mapNotFound(err)
	}
	existing, lookupErr := scanThreadDraft(tx.QueryRow(ctx, threadDraftSelect+`
		WHERE telegram_id = $1 AND generation_update_id = $2`, draft.TelegramID, draft.GenerationUpdateID))
	if lookupErr == nil {
		if existing.BriefID != briefID {
			return domain.ThreadDraft{}, false, ErrThreadBriefState
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.ThreadDraft{}, false, err
		}
		return existing, false, nil
	}
	if !errors.Is(lookupErr, pgx.ErrNoRows) {
		return domain.ThreadDraft{}, false, lookupErr
	}
	brief, err := scanThreadBrief(tx.QueryRow(ctx, threadBriefSelect+`
		WHERE id = $1 AND telegram_id = $2 FOR UPDATE`, briefID, draft.TelegramID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ThreadDraft{}, false, ErrNotFound
	}
	if err != nil {
		return domain.ThreadDraft{}, false, err
	}
	if !brief.Current || brief.Revision != briefRevision || brief.State != domain.ThreadBriefMaterialReady {
		replayed, replayErr := scanThreadDraft(tx.QueryRow(ctx, threadDraftSelect+`
			WHERE telegram_id = $1 AND generation_update_id = $2`, draft.TelegramID, draft.GenerationUpdateID))
		if replayErr == nil && replayed.BriefID == briefID {
			if err := tx.Commit(ctx); err != nil {
				return domain.ThreadDraft{}, false, err
			}
			return replayed, false, nil
		}
		if replayErr != nil && !errors.Is(replayErr, pgx.ErrNoRows) {
			return domain.ThreadDraft{}, false, replayErr
		}
		return domain.ThreadDraft{}, false, ErrThreadBriefState
	}
	if draft.Voice != brief.Voice {
		return domain.ThreadDraft{}, false, ErrThreadBriefState
	}
	if draft.BriefID != 0 && draft.BriefID != briefID {
		return domain.ThreadDraft{}, false, ErrThreadBriefState
	}
	draft.BriefID = briefID
	draft.Objective = brief.Objective
	if draft.MediaMode == "" {
		draft.MediaMode = domain.ThreadMediaText
	}
	var mediaValue domain.ThreadMedia
	if mediaInput != nil {
		mediaValue = *mediaInput
		if draft.MediaMode != domain.ThreadMediaImage || draft.MediaID != 0 ||
			mediaValue.TelegramID != draft.TelegramID || mediaValue.AttachUpdateID != 0 {
			return domain.ThreadDraft{}, false, ErrThreadDraftState
		}
		if err := mediaValue.ValidateForStore(); err != nil {
			return domain.ThreadDraft{}, false, err
		}
		validationDraft := draft
		validationDraft.MediaID = 1
		if err := validationDraft.ValidateForCreate(); err != nil {
			return domain.ThreadDraft{}, false, err
		}
	} else if err := draft.ValidateForCreate(); err != nil {
		return domain.ThreadDraft{}, false, err
	}
	var mediaID int64
	if mediaInput != nil {
		mediaValue.SourceKind = mediaValue.EffectiveSourceKind()
		if err := tx.QueryRow(ctx, `
			INSERT INTO thread_media (
				telegram_id, source_kind, source_update_id, attach_update_id, source_asset_id,
				source_page_url, source_author, source_author_url, source_query,
				content, media_type, width, height, digest, delivery_key
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			RETURNING id`,
			mediaValue.TelegramID, mediaValue.SourceKind, nullablePositiveInt64(mediaValue.SourceUpdateID),
			nullablePositiveInt64(mediaValue.AttachUpdateID), mediaValue.SourceAssetID, mediaValue.SourcePageURL,
			mediaValue.SourceAuthor, mediaValue.SourceAuthorURL, mediaValue.SourceQuery, mediaValue.Data,
			mediaValue.MediaType, mediaValue.Width, mediaValue.Height, mediaValue.Digest, mediaValue.DeliveryKey,
		).Scan(&mediaID); err != nil {
			return domain.ThreadDraft{}, false, err
		}
		draft.MediaID = mediaID
	}
	if _, err := tx.Exec(ctx, `
		UPDATE thread_drafts SET is_current = FALSE, updated_at = now()
		WHERE telegram_id = $1 AND is_current`, draft.TelegramID); err != nil {
		return domain.ThreadDraft{}, false, err
	}
	created, err := scanThreadDraft(tx.QueryRow(ctx, `
		INSERT INTO thread_drafts (
			telegram_id, voice, goal, brief_id, objective, scenario_id, generation_id,
			generation_update_id, photo_query, preview_text, provider, model, revision,
			media_mode, media_id, state, is_current
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, 'draft', TRUE)
		RETURNING `+threadDraftColumns,
		draft.TelegramID, draft.Voice, draft.Goal, briefID, draft.Objective, draft.ScenarioID,
		draft.GenerationID, draft.GenerationUpdateID, draft.PhotoQuery, draft.Text, draft.Provider,
		draft.Model, draft.Revision, draft.MediaMode, nullablePositiveInt64(draft.MediaID),
	))
	if err != nil {
		return domain.ThreadDraft{}, false, err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE thread_briefs
		SET state = 'draft_ready', revision = revision + 1, error_code = '', updated_at = now()
		WHERE id = $1 AND telegram_id = $2 AND revision = $3 AND is_current
		  AND state = 'material_ready'`, briefID, draft.TelegramID, briefRevision)
	if err != nil {
		return domain.ThreadDraft{}, false, err
	}
	if tag.RowsAffected() != 1 {
		return domain.ThreadDraft{}, false, ErrThreadBriefState
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ThreadDraft{}, false, err
	}
	return created, true, nil
}

func (p *Postgres) GetThreadDraftByGenerationUpdate(ctx context.Context, telegramID, updateID int64) (domain.ThreadDraft, error) {
	if updateID <= 0 {
		return domain.ThreadDraft{}, ErrNotFound
	}
	draft, err := scanThreadDraft(p.pool.QueryRow(ctx, threadDraftSelect+`
		WHERE telegram_id = $1 AND generation_update_id = $2`, telegramID, updateID))
	if err != nil {
		return domain.ThreadDraft{}, mapNotFound(err)
	}
	return draft, nil
}

func (p *Postgres) CancelThreadBrief(ctx context.Context, id, telegramID int64, revision uint32) error {
	if revision == ^uint32(0) {
		return ErrThreadBriefState
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	tag, err := tx.Exec(ctx, `
		UPDATE thread_briefs
		SET state = 'cancelled', is_current = FALSE, revision = revision + 1,
			error_code = '', updated_at = now()
		WHERE id = $1 AND telegram_id = $2 AND revision = $3 AND is_current
		  AND state <> 'cancelled'`, id, telegramID, revision)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		brief, lookupErr := scanThreadBrief(tx.QueryRow(ctx, threadBriefSelect+`
			WHERE id = $1 AND telegram_id = $2`, id, telegramID))
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if lookupErr != nil {
			return lookupErr
		}
		if brief.State == domain.ThreadBriefCancelled && brief.Revision == revision+1 {
			return tx.Commit(ctx)
		}
		return ErrThreadBriefState
	}
	if _, err := tx.Exec(ctx, `
		UPDATE thread_drafts
		SET state = 'cancelled', is_current = FALSE, error_code = '', updated_at = now()
		WHERE brief_id = $1 AND telegram_id = $2 AND is_current
		  AND state IN ('draft', 'failed')`, id, telegramID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) CreateThreadDraft(ctx context.Context, draft domain.ThreadDraft) (int64, error) {
	draft = normalizeLegacyThreadDraft(draft)
	if err := draft.ValidateForCreate(); err != nil {
		return 0, err
	}
	if draft.MediaMode == "" {
		draft.MediaMode = domain.ThreadMediaText
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	var owner int64
	if err := tx.QueryRow(ctx, `SELECT telegram_id FROM users WHERE telegram_id = $1 FOR UPDATE`, draft.TelegramID).Scan(&owner); err != nil {
		return 0, mapNotFound(err)
	}
	if draft.BriefID > 0 {
		var briefOwner int64
		var objective domain.ThreadObjective
		var voice domain.ThreadVoice
		var state domain.ThreadBriefState
		var current bool
		if err := tx.QueryRow(ctx, `
			SELECT telegram_id, objective, voice, state, is_current
			FROM thread_briefs WHERE id = $1 FOR SHARE`, draft.BriefID,
		).Scan(&briefOwner, &objective, &voice, &state, &current); err != nil {
			return 0, mapNotFound(err)
		}
		if briefOwner != draft.TelegramID {
			return 0, ErrNotFound
		}
		if !current || state != domain.ThreadBriefDraftReady || voice != draft.Voice || objective != draft.Objective {
			return 0, ErrThreadBriefState
		}
	}
	if draft.MediaID > 0 {
		var mediaOwner int64
		if err := tx.QueryRow(ctx, `SELECT telegram_id FROM thread_media WHERE id = $1 FOR SHARE`, draft.MediaID).Scan(&mediaOwner); err != nil {
			return 0, mapNotFound(err)
		}
		if mediaOwner != draft.TelegramID {
			return 0, ErrNotFound
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE thread_drafts SET is_current = FALSE, updated_at = now()
		WHERE telegram_id = $1 AND is_current`, draft.TelegramID); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO thread_drafts (
			telegram_id, voice, goal, brief_id, objective, scenario_id, generation_id,
			generation_update_id, photo_query, preview_text, provider, model,
			revision, media_mode, media_id, state, is_current
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, 'draft', TRUE)
		RETURNING id`,
		draft.TelegramID, draft.Voice, draft.Goal, nullablePositiveInt64(draft.BriefID), draft.Objective,
		draft.ScenarioID, draft.GenerationID, nullablePositiveInt64(draft.GenerationUpdateID), draft.PhotoQuery,
		draft.Text, draft.Provider, draft.Model, draft.Revision, draft.MediaMode, nullablePositiveInt64(draft.MediaID),
	).Scan(&id); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return id, nil
}

func (p *Postgres) CreateThreadDraftWithMedia(ctx context.Context, draft domain.ThreadDraft, mediaValue domain.ThreadMedia) (int64, error) {
	draft = normalizeLegacyThreadDraft(draft)
	if draft.MediaMode != domain.ThreadMediaImage || draft.MediaID != 0 || mediaValue.TelegramID != draft.TelegramID ||
		mediaValue.AttachUpdateID != 0 {
		return 0, ErrThreadDraftState
	}
	if err := mediaValue.ValidateForStore(); err != nil {
		return 0, err
	}
	validationDraft := draft
	validationDraft.MediaID = 1
	if err := validationDraft.ValidateForCreate(); err != nil {
		return 0, err
	}
	mediaValue.SourceKind = mediaValue.EffectiveSourceKind()

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	var owner int64
	if err := tx.QueryRow(ctx, `SELECT telegram_id FROM users WHERE telegram_id = $1 FOR UPDATE`, draft.TelegramID).Scan(&owner); err != nil {
		return 0, mapNotFound(err)
	}
	if draft.BriefID > 0 {
		var briefOwner int64
		var objective domain.ThreadObjective
		var voice domain.ThreadVoice
		var state domain.ThreadBriefState
		var current bool
		if err := tx.QueryRow(ctx, `
			SELECT telegram_id, objective, voice, state, is_current
			FROM thread_briefs WHERE id = $1 FOR SHARE`, draft.BriefID,
		).Scan(&briefOwner, &objective, &voice, &state, &current); err != nil {
			return 0, mapNotFound(err)
		}
		if briefOwner != draft.TelegramID {
			return 0, ErrNotFound
		}
		if !current || state != domain.ThreadBriefDraftReady || voice != draft.Voice || objective != draft.Objective {
			return 0, ErrThreadBriefState
		}
	}
	var mediaID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO thread_media (
			telegram_id, source_kind, source_update_id, attach_update_id, source_asset_id,
			source_page_url, source_author, source_author_url, source_query,
			content, media_type, width, height, digest, delivery_key
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING id`,
		mediaValue.TelegramID, mediaValue.SourceKind, nullablePositiveInt64(mediaValue.SourceUpdateID), nullablePositiveInt64(mediaValue.AttachUpdateID), mediaValue.SourceAssetID,
		mediaValue.SourcePageURL, mediaValue.SourceAuthor, mediaValue.SourceAuthorURL, mediaValue.SourceQuery,
		mediaValue.Data, mediaValue.MediaType, mediaValue.Width, mediaValue.Height, mediaValue.Digest, mediaValue.DeliveryKey,
	).Scan(&mediaID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE thread_drafts SET is_current = FALSE, updated_at = now()
		WHERE telegram_id = $1 AND is_current`, draft.TelegramID); err != nil {
		return 0, err
	}
	var draftID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO thread_drafts (
			telegram_id, voice, goal, brief_id, objective, scenario_id, generation_id,
			generation_update_id, photo_query, preview_text, provider, model,
			revision, media_mode, media_id, state, is_current
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'image', $14, 'draft', TRUE)
		RETURNING id`,
		draft.TelegramID, draft.Voice, draft.Goal, nullablePositiveInt64(draft.BriefID), draft.Objective,
		draft.ScenarioID, draft.GenerationID, nullablePositiveInt64(draft.GenerationUpdateID), draft.PhotoQuery,
		draft.Text, draft.Provider, draft.Model, draft.Revision, mediaID,
	).Scan(&draftID); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return draftID, nil
}

func (p *Postgres) GetThreadDraft(ctx context.Context, id, telegramID int64) (domain.ThreadDraft, error) {
	draft, err := scanThreadDraft(p.pool.QueryRow(ctx, threadDraftSelect+` WHERE id = $1 AND telegram_id = $2`, id, telegramID))
	if err != nil {
		return domain.ThreadDraft{}, mapNotFound(err)
	}
	return draft, nil
}

func (p *Postgres) GetCurrentThreadDraft(ctx context.Context, telegramID int64) (domain.ThreadDraft, error) {
	draft, err := scanThreadDraft(p.pool.QueryRow(ctx, threadDraftSelect+` WHERE telegram_id = $1 AND is_current`, telegramID))
	if err != nil {
		return domain.ThreadDraft{}, mapNotFound(err)
	}
	return draft, nil
}

func (p *Postgres) SetThreadDraftMediaMode(
	ctx context.Context,
	id, telegramID int64,
	revision uint32,
	mode domain.ThreadMediaMode,
) (domain.ThreadDraft, error) {
	if mode != domain.ThreadMediaText && mode != domain.ThreadMediaImagePending && mode != domain.ThreadMediaImage {
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return domain.ThreadDraft{}, err
	}
	defer rollback(tx)
	draft, err := scanThreadDraft(tx.QueryRow(ctx, `
		UPDATE thread_drafts
		SET revision = revision + 1,
			media_mode = $5,
			media_id = CASE WHEN $5 = 'text' THEN NULL ELSE media_id END,
			media_rights_confirmed_at = NULL,
			state = 'draft', container_id = '', post_id = '', permalink = '',
			error_code = '', claim_token = '', claim_expires_at = NULL,
			publish_started_at = NULL, published_at = NULL, updated_at = now()
		WHERE id = $1 AND telegram_id = $2 AND revision = $3
		  AND is_current AND revision < $4 AND state IN ('draft', 'failed')
		  AND ($5 <> 'image' OR (media_mode = 'image_pending' AND media_id IS NOT NULL))
		RETURNING `+threadDraftColumns,
		id, telegramID, revision, int64(^uint32(0)), mode,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, lookupErr := scanThreadDraft(tx.QueryRow(
			ctx, threadDraftSelect+` WHERE id = $1 AND telegram_id = $2`, id, telegramID,
		)); lookupErr != nil {
			return domain.ThreadDraft{}, mapNotFound(lookupErr)
		}
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	if err != nil {
		return domain.ThreadDraft{}, err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM thread_media AS media
		WHERE media.telegram_id = $1
		  AND NOT EXISTS (SELECT 1 FROM thread_drafts AS draft WHERE draft.media_id = media.id)`, telegramID); err != nil {
		return domain.ThreadDraft{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ThreadDraft{}, err
	}
	return draft, nil
}

func (p *Postgres) AttachThreadDraftMedia(
	ctx context.Context,
	id, telegramID int64,
	revision uint32,
	mediaValue domain.ThreadMedia,
) (domain.ThreadDraft, error) {
	if mediaValue.TelegramID != telegramID {
		return domain.ThreadDraft{}, ErrNotFound
	}
	if mediaValue.EffectiveSourceKind() != domain.ThreadMediaSourceTelegram {
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	if err := mediaValue.ValidateForStore(); err != nil {
		return domain.ThreadDraft{}, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return domain.ThreadDraft{}, err
	}
	defer rollback(tx)
	var mediaID int64
	insertErr := tx.QueryRow(ctx, `
		INSERT INTO thread_media (
			telegram_id, source_kind, source_update_id, content, media_type,
			width, height, digest, delivery_key
		) VALUES ($1, 'telegram_upload', $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (telegram_id, source_update_id) DO NOTHING
		RETURNING id`,
		mediaValue.TelegramID, mediaValue.SourceUpdateID, mediaValue.Data, mediaValue.MediaType,
		mediaValue.Width, mediaValue.Height, mediaValue.Digest, mediaValue.DeliveryKey,
	).Scan(&mediaID)
	if errors.Is(insertErr, pgx.ErrNoRows) {
		// The Telegram update was already committed. It is an idempotent retry
		// only for the exact draft that owns that attachment; never move a replay
		// to a later draft merely because the owner is the same.
		if err := tx.QueryRow(ctx, `
			SELECT id FROM thread_media
			WHERE telegram_id = $1 AND source_kind = 'telegram_upload' AND source_update_id = $2
			FOR SHARE`, telegramID, mediaValue.SourceUpdateID).Scan(&mediaID); err != nil {
			return domain.ThreadDraft{}, mapNotFound(err)
		}
		draft, err := scanThreadDraft(tx.QueryRow(ctx, threadDraftSelect+`
			WHERE id = $1 AND telegram_id = $2 AND media_id = $3`, id, telegramID, mediaID))
		if err == nil {
			return draft, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.ThreadDraft{}, err
		}
		if _, lookupErr := scanThreadDraft(tx.QueryRow(
			ctx, threadDraftSelect+` WHERE id = $1 AND telegram_id = $2`, id, telegramID,
		)); lookupErr != nil {
			return domain.ThreadDraft{}, mapNotFound(lookupErr)
		}
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	if insertErr != nil {
		return domain.ThreadDraft{}, insertErr
	}
	draft, err := scanThreadDraft(tx.QueryRow(ctx, `
		UPDATE thread_drafts
		SET revision = revision + 1, media_mode = 'image', media_id = $5,
			media_rights_confirmed_at = NULL,
			state = 'draft', container_id = '', post_id = '', permalink = '',
			error_code = '', claim_token = '', claim_expires_at = NULL,
			publish_started_at = NULL, published_at = NULL, updated_at = now()
		WHERE id = $1 AND telegram_id = $2 AND revision = $3
		  AND is_current AND revision < $4 AND media_mode = 'image_pending'
		  AND state IN ('draft', 'failed')
		RETURNING `+threadDraftColumns,
		id, telegramID, revision, int64(^uint32(0)), mediaID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, lookupErr := scanThreadDraft(tx.QueryRow(
			ctx, threadDraftSelect+` WHERE id = $1 AND telegram_id = $2`, id, telegramID,
		)); lookupErr != nil {
			return domain.ThreadDraft{}, mapNotFound(lookupErr)
		}
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	if err != nil {
		return domain.ThreadDraft{}, err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM thread_media AS media
		WHERE media.telegram_id = $1
		  AND NOT EXISTS (SELECT 1 FROM thread_drafts AS current_draft WHERE current_draft.media_id = media.id)`, telegramID); err != nil {
		return domain.ThreadDraft{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ThreadDraft{}, err
	}
	return draft, nil
}

func (p *Postgres) AttachLicensedThreadDraftMedia(
	ctx context.Context,
	id, telegramID int64,
	revision uint32,
	mediaValue domain.ThreadMedia,
) (domain.ThreadDraft, error) {
	if mediaValue.TelegramID != telegramID {
		return domain.ThreadDraft{}, ErrNotFound
	}
	if mediaValue.EffectiveSourceKind() != domain.ThreadMediaSourcePexels || mediaValue.AttachUpdateID <= 0 {
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	if err := mediaValue.ValidateForStore(); err != nil {
		return domain.ThreadDraft{}, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return domain.ThreadDraft{}, err
	}
	defer rollback(tx)
	var existingDraftID int64
	var existingMediaID sql.NullInt64
	operationErr := tx.QueryRow(ctx, `
		SELECT draft_id, media_id
		FROM thread_media_attach_operations
		WHERE telegram_id = $1 AND attach_update_id = $2
		FOR SHARE`, telegramID, mediaValue.AttachUpdateID,
	).Scan(&existingDraftID, &existingMediaID)
	if operationErr == nil {
		if existingDraftID != id || !existingMediaID.Valid {
			return domain.ThreadDraft{}, ErrThreadDraftState
		}
		draft, lookupErr := scanThreadDraft(tx.QueryRow(ctx, threadDraftSelect+`
			WHERE id = $1 AND telegram_id = $2 AND media_id = $3`, id, telegramID, existingMediaID.Int64))
		if lookupErr == nil {
			return draft, nil
		}
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			return domain.ThreadDraft{}, ErrThreadDraftState
		}
		return domain.ThreadDraft{}, lookupErr
	}
	if !errors.Is(operationErr, pgx.ErrNoRows) {
		return domain.ThreadDraft{}, operationErr
	}
	var mediaID int64
	insertErr := tx.QueryRow(ctx, `
		INSERT INTO thread_media (
			telegram_id, source_kind, source_update_id, attach_update_id,
			source_asset_id, source_page_url, source_author, source_author_url, source_query,
			content, media_type, width, height, digest, delivery_key
		) VALUES ($1, 'pexels', NULL, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (telegram_id, attach_update_id) WHERE attach_update_id IS NOT NULL DO NOTHING
		RETURNING id`,
		mediaValue.TelegramID, mediaValue.AttachUpdateID, mediaValue.SourceAssetID,
		mediaValue.SourcePageURL, mediaValue.SourceAuthor, mediaValue.SourceAuthorURL, mediaValue.SourceQuery,
		mediaValue.Data, mediaValue.MediaType, mediaValue.Width, mediaValue.Height, mediaValue.Digest, mediaValue.DeliveryKey,
	).Scan(&mediaID)
	if errors.Is(insertErr, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx, `
			SELECT id FROM thread_media
			WHERE telegram_id = $1 AND source_kind = 'pexels' AND attach_update_id = $2
			FOR SHARE`, telegramID, mediaValue.AttachUpdateID).Scan(&mediaID); err != nil {
			return domain.ThreadDraft{}, mapNotFound(err)
		}
		draft, err := scanThreadDraft(tx.QueryRow(ctx, threadDraftSelect+`
			WHERE id = $1 AND telegram_id = $2 AND media_id = $3`, id, telegramID, mediaID))
		if err == nil {
			return draft, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.ThreadDraft{}, err
		}
		if _, lookupErr := scanThreadDraft(tx.QueryRow(
			ctx, threadDraftSelect+` WHERE id = $1 AND telegram_id = $2`, id, telegramID,
		)); lookupErr != nil {
			return domain.ThreadDraft{}, mapNotFound(lookupErr)
		}
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	if insertErr != nil {
		return domain.ThreadDraft{}, insertErr
	}
	draft, err := scanThreadDraft(tx.QueryRow(ctx, `
		UPDATE thread_drafts
		SET revision = revision + 1, photo_query = $6, media_mode = 'image', media_id = $5,
			media_rights_confirmed_at = NULL,
			state = 'draft', container_id = '', post_id = '', permalink = '',
			error_code = '', claim_token = '', claim_expires_at = NULL,
			publish_started_at = NULL, published_at = NULL, updated_at = now()
		WHERE id = $1 AND telegram_id = $2 AND revision = $3
		  AND is_current AND revision < $4 AND state IN ('draft', 'failed')
		  AND NOT EXISTS (
			SELECT 1 FROM thread_media AS current_media
			WHERE current_media.id = thread_drafts.media_id
			  AND (current_media.source_asset_id = $7 OR current_media.digest = $8)
		  )
		RETURNING `+threadDraftColumns,
		id, telegramID, revision, int64(^uint32(0)), mediaID, mediaValue.SourceQuery,
		mediaValue.SourceAssetID, mediaValue.Digest,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, lookupErr := scanThreadDraft(tx.QueryRow(
			ctx, threadDraftSelect+` WHERE id = $1 AND telegram_id = $2`, id, telegramID,
		)); lookupErr != nil {
			return domain.ThreadDraft{}, mapNotFound(lookupErr)
		}
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	if err != nil {
		return domain.ThreadDraft{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO thread_media_attach_operations (
			telegram_id, attach_update_id, draft_id, media_id
		) VALUES ($1, $2, $3, $4)`,
		telegramID, mediaValue.AttachUpdateID, id, mediaID,
	); err != nil {
		return domain.ThreadDraft{}, err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM thread_media AS media
		WHERE media.telegram_id = $1
		  AND NOT EXISTS (SELECT 1 FROM thread_drafts AS current_draft WHERE current_draft.media_id = media.id)`, telegramID); err != nil {
		return domain.ThreadDraft{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ThreadDraft{}, err
	}
	return draft, nil
}

func (p *Postgres) GetThreadMedia(ctx context.Context, id, telegramID int64) (domain.ThreadMedia, error) {
	mediaValue, err := scanThreadMedia(p.pool.QueryRow(ctx, threadMediaSelect+` WHERE id = $1 AND telegram_id = $2`, id, telegramID))
	if err != nil {
		return domain.ThreadMedia{}, mapNotFound(err)
	}
	return mediaValue, nil
}

func (p *Postgres) GetThreadMediaByDeliveryKey(ctx context.Context, deliveryKey string) (domain.ThreadMedia, error) {
	if err := validateThreadField("media delivery key", deliveryKey, 32, true); err != nil {
		return domain.ThreadMedia{}, ErrNotFound
	}
	mediaValue, err := scanThreadMedia(p.pool.QueryRow(ctx, threadMediaSelect+` WHERE delivery_key = $1`, deliveryKey))
	if err != nil {
		return domain.ThreadMedia{}, mapNotFound(err)
	}
	return mediaValue, nil
}

func (p *Postgres) GetThreadDraftByMediaUpdate(ctx context.Context, telegramID, sourceUpdateID int64) (domain.ThreadDraft, error) {
	draft, err := scanThreadDraft(p.pool.QueryRow(ctx, threadDraftSelect+`
		WHERE telegram_id = $1
		  AND media_id = (
			SELECT id FROM thread_media
			WHERE telegram_id = $1 AND source_kind = 'telegram_upload' AND source_update_id = $2
		  )
		ORDER BY is_current DESC, updated_at DESC, id DESC LIMIT 1`, telegramID, sourceUpdateID))
	if err != nil {
		return domain.ThreadDraft{}, mapNotFound(err)
	}
	return draft, nil
}

func (p *Postgres) GetThreadDraftByMediaAttachUpdate(ctx context.Context, telegramID, attachUpdateID int64) (domain.ThreadDraft, error) {
	if attachUpdateID <= 0 {
		return domain.ThreadDraft{}, ErrNotFound
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return domain.ThreadDraft{}, err
	}
	defer rollback(tx)
	var draftID int64
	var mediaID sql.NullInt64
	if err := tx.QueryRow(ctx, `
		SELECT draft_id, media_id
		FROM thread_media_attach_operations
		WHERE telegram_id = $1 AND attach_update_id = $2
		FOR SHARE`, telegramID, attachUpdateID,
	).Scan(&draftID, &mediaID); err != nil {
		return domain.ThreadDraft{}, mapNotFound(err)
	}
	if !mediaID.Valid {
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	draft, err := scanThreadDraft(tx.QueryRow(ctx, threadDraftSelect+`
		WHERE id = $1 AND telegram_id = $2 AND media_id = $3`, draftID, telegramID, mediaID.Int64))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ThreadDraft{}, ErrThreadDraftState
	}
	if err != nil {
		return domain.ThreadDraft{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ThreadDraft{}, err
	}
	return draft, nil
}

func (p *Postgres) ListRecentThreadTexts(ctx context.Context, telegramID int64, limit int) ([]string, error) {
	if limit <= 0 {
		return []string{}, nil
	}
	rows, err := p.pool.Query(ctx, `
		SELECT preview_text FROM thread_drafts
		WHERE telegram_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2`, telegramID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	texts := make([]string, 0, limit)
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		texts = append(texts, value)
	}
	return texts, rows.Err()
}

func (p *Postgres) ClaimThreadDraft(
	ctx context.Context,
	id, telegramID int64,
	revision uint32,
	claimToken string,
	now time.Time,
	lease time.Duration,
) (domain.ThreadDraft, bool, error) {
	if err := validateThreadClaim(claimToken, now, lease); err != nil {
		return domain.ThreadDraft{}, false, err
	}
	now = now.UTC()
	expiresAt := now.Add(lease)
	draft, err := scanThreadDraft(p.pool.QueryRow(ctx, `
		UPDATE thread_drafts
		SET state = CASE
				WHEN state = 'publishing' AND publish_started_at IS NOT NULL THEN 'unknown'
				ELSE 'publishing'
			END,
			error_code = CASE
				WHEN state = 'publishing' AND publish_started_at IS NOT NULL THEN 'publish_lease_expired'
				ELSE ''
			END,
			claim_token = CASE
				WHEN state = 'publishing' AND publish_started_at IS NOT NULL THEN ''
				ELSE $4
			END,
			claim_expires_at = CASE
				WHEN state = 'publishing' AND publish_started_at IS NOT NULL THEN NULL
				ELSE $6::timestamptz
			END,
			publish_started_at = CASE
				WHEN state = 'publishing' AND publish_started_at IS NOT NULL THEN publish_started_at
				ELSE NULL
			END,
			media_rights_confirmed_at = CASE
				WHEN state = 'publishing' AND publish_started_at IS NOT NULL THEN media_rights_confirmed_at
				WHEN media_mode = 'image' THEN $5
				ELSE NULL
			END,
			updated_at = $5
		WHERE id = $1 AND telegram_id = $2 AND revision = $3
		  AND is_current
		  AND (media_mode = 'text' OR (media_mode = 'image' AND media_id IS NOT NULL))
		  AND (
			state IN ('draft', 'failed')
			OR (
				state = 'publishing'
				AND (claim_expires_at IS NULL OR claim_expires_at <= $5)
			)
		  )
		RETURNING `+threadDraftColumns, id, telegramID, revision, claimToken, now, expiresAt))
	if err == nil {
		return draft, draft.State == domain.ThreadDraftPublishing && draft.ClaimToken == claimToken, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return domain.ThreadDraft{}, false, err
	}
	draft, err = p.GetThreadDraft(ctx, id, telegramID)
	if err != nil {
		return domain.ThreadDraft{}, false, err
	}
	return draft, false, nil
}

func (p *Postgres) SetThreadContainer(ctx context.Context, id, telegramID int64, claimToken, containerID string) error {
	if err := validateThreadField("claim token", claimToken, maxThreadClaimTokenRunes, true); err != nil {
		return err
	}
	if err := validateThreadField("container id", containerID, 255, true); err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE thread_drafts
		SET container_id = $4, updated_at = now()
		WHERE id = $1 AND telegram_id = $2 AND state = 'publishing'
		  AND claim_token = $3 AND claim_expires_at > now()
		  AND publish_started_at IS NULL
		  AND (container_id = '' OR container_id = $4)`, id, telegramID, claimToken, containerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	draft, err := p.GetThreadDraft(ctx, id, telegramID)
	if err != nil {
		return err
	}
	if draft.State == domain.ThreadDraftPublishing && draft.ClaimToken == claimToken &&
		draft.PublishStartedAt == nil && draft.ContainerID == containerID {
		return nil
	}
	return ErrThreadDraftState
}

func (p *Postgres) BeginThreadPublish(
	ctx context.Context,
	id, telegramID int64,
	claimToken string,
	now time.Time,
	lease time.Duration,
) error {
	if err := validateThreadClaim(claimToken, now, lease); err != nil {
		return err
	}
	now = now.UTC()
	tag, err := p.pool.Exec(ctx, `
		UPDATE thread_drafts
		SET publish_started_at = $4, claim_expires_at = $5, updated_at = $4
		WHERE id = $1 AND telegram_id = $2 AND state = 'publishing'
		  AND claim_token = $3 AND claim_expires_at > $4
		  AND container_id <> '' AND publish_started_at IS NULL`,
		id, telegramID, claimToken, now, now.Add(lease))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := p.GetThreadDraft(ctx, id, telegramID); err != nil {
		return err
	}
	return ErrThreadDraftState
}

func (p *Postgres) CompleteThreadDraft(ctx context.Context, id, telegramID int64, claimToken, postID, permalink string) error {
	if err := validateThreadField("claim token", claimToken, maxThreadClaimTokenRunes, true); err != nil {
		return err
	}
	if err := validateThreadField("post id", postID, 255, false); err != nil {
		return err
	}
	if err := validateThreadField("permalink", permalink, 2048, false); err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE thread_drafts
		SET state = 'published', post_id = $4, permalink = $5,
			error_code = '', claim_token = '', claim_expires_at = NULL,
			published_at = now(), updated_at = now()
		WHERE id = $1 AND telegram_id = $2 AND state = 'publishing'
		  AND claim_token = $3 AND publish_started_at IS NOT NULL`,
		id, telegramID, claimToken, postID, permalink)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	draft, err := p.GetThreadDraft(ctx, id, telegramID)
	if err != nil {
		return err
	}
	if draft.State == domain.ThreadDraftPublished && draft.PostID == postID && draft.Permalink == permalink {
		return nil
	}
	return ErrThreadDraftState
}

func (p *Postgres) ConfirmThreadDraftPublished(ctx context.Context, id, telegramID int64, containerID string) error {
	if err := validateThreadField("container id", containerID, 255, true); err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE thread_drafts
		SET state = 'published', error_code = '', claim_token = '', claim_expires_at = NULL,
			published_at = now(), updated_at = now()
		WHERE id = $1 AND telegram_id = $2 AND container_id = $3
		  AND (state = 'unknown' OR (state = 'publishing' AND publish_started_at IS NOT NULL))`,
		id, telegramID, containerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	draft, err := p.GetThreadDraft(ctx, id, telegramID)
	if err != nil {
		return err
	}
	if draft.State == domain.ThreadDraftPublished && draft.ContainerID == containerID {
		return nil
	}
	return ErrThreadDraftState
}

func (p *Postgres) FailThreadDraft(ctx context.Context, id, telegramID int64, claimToken, errorCode string, unknown bool) error {
	if err := validateThreadField("claim token", claimToken, maxThreadClaimTokenRunes, true); err != nil {
		return err
	}
	errorCode = normalizeThreadErrorCode(errorCode)
	target := domain.ThreadDraftFailed
	if unknown {
		target = domain.ThreadDraftUnknown
	}
	tag, err := p.pool.Exec(ctx, `
		UPDATE thread_drafts
		SET state = $4, error_code = $5, claim_token = '', claim_expires_at = NULL,
			container_id = CASE WHEN $4 = 'failed' THEN '' ELSE container_id END,
			publish_started_at = CASE WHEN $4 = 'failed' THEN NULL ELSE publish_started_at END,
			updated_at = now()
		WHERE id = $1 AND telegram_id = $2 AND state = 'publishing'
		  AND claim_token = $3
		  AND ($4 <> 'unknown' OR publish_started_at IS NOT NULL)`,
		id, telegramID, claimToken, target, errorCode)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	draft, err := p.GetThreadDraft(ctx, id, telegramID)
	if err != nil {
		return err
	}
	if draft.State == target && draft.ErrorCode == errorCode {
		return nil
	}
	return ErrThreadDraftState
}

func (p *Postgres) CancelThreadDraft(ctx context.Context, id, telegramID int64, revision uint32) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	var briefID sql.NullInt64
	if err := tx.QueryRow(ctx, `
		SELECT brief_id FROM thread_drafts WHERE id = $1 AND telegram_id = $2`, id, telegramID,
	).Scan(&briefID); err != nil {
		return mapNotFound(err)
	}
	if briefID.Valid {
		var lockedBriefID int64
		if err := tx.QueryRow(ctx, `
			SELECT id FROM thread_briefs WHERE id = $1 AND telegram_id = $2 FOR UPDATE`,
			briefID.Int64, telegramID,
		).Scan(&lockedBriefID); errors.Is(err, pgx.ErrNoRows) {
			// Retention may have removed the brief and unlinked the draft after
			// the metadata read. Cancelling the still-owned draft remains safe.
			briefID.Valid = false
		} else if err != nil {
			return err
		}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE thread_drafts
		SET state = 'cancelled', is_current = FALSE, error_code = '', updated_at = now()
		WHERE id = $1 AND telegram_id = $2 AND revision = $3
		  AND is_current AND state IN ('draft', 'failed')`, id, telegramID, revision)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, lookupErr := scanThreadDraft(tx.QueryRow(ctx, threadDraftSelect+`
			WHERE id = $1 AND telegram_id = $2`, id, telegramID)); lookupErr != nil {
			return mapNotFound(lookupErr)
		}
		return ErrNotFound
	}
	if briefID.Valid {
		if _, err := tx.Exec(ctx, `
			UPDATE thread_briefs
			SET state = 'cancelled', is_current = FALSE,
				revision = CASE WHEN revision < $3 THEN revision + 1 ELSE revision END,
				error_code = '', updated_at = now()
			WHERE id = $1 AND telegram_id = $2 AND is_current AND state = 'draft_ready'`,
			briefID.Int64, telegramID, int64(^uint32(0))); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (p *Postgres) RecordFeedback(ctx context.Context, feedback domain.Feedback) error {
	tag, err := p.pool.Exec(ctx, `
		INSERT INTO feedback (generation_id, telegram_id, candidate, rating, action)
		SELECT $1, $2, $3, $4, $5
		WHERE EXISTS (SELECT 1 FROM generations WHERE id = $1 AND telegram_id = $2)
		ON CONFLICT (generation_id, telegram_id, candidate, action) DO UPDATE SET rating = EXCLUDED.rating`,
		feedback.GenerationID, feedback.TelegramID, feedback.Candidate, feedback.Rating, feedback.Action)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (p *Postgres) ListStyleExamples(ctx context.Context, telegramID int64, limit int) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT example_text FROM style_examples WHERE telegram_id = $1 ORDER BY created_at DESC LIMIT $2`, telegramID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (p *Postgres) SaveStyleExample(ctx context.Context, telegramID, generationID int64, text string, limit int) (bool, error) {
	if limit < 1 {
		return false, nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(tx)
	var lockedID int64
	if err := tx.QueryRow(ctx, `SELECT telegram_id FROM users WHERE telegram_id = $1 FOR UPDATE`, telegramID).Scan(&lockedID); err != nil {
		return false, mapNotFound(err)
	}
	var owned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM generations WHERE id = $1 AND telegram_id = $2)`, generationID, telegramID).Scan(&owned); err != nil {
		return false, err
	}
	if !owned {
		return false, ErrNotFound
	}
	var duplicate bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM style_examples WHERE telegram_id = $1 AND example_text = $2
	)`, telegramID, text).Scan(&duplicate); err != nil {
		return false, err
	}
	if duplicate {
		return true, tx.Commit(ctx)
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM style_examples WHERE telegram_id = $1`, telegramID).Scan(&count); err != nil {
		return false, err
	}
	if count >= limit {
		return false, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO style_examples (telegram_id, source_generation_id, example_text)
		VALUES ($1, $2, $3)`, telegramID, generationID, text); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (p *Postgres) ResetStyle(ctx context.Context, telegramID int64) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `DELETE FROM style_examples WHERE telegram_id = $1`, telegramID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE users SET default_tone = 'mix', updated_at = now() WHERE telegram_id = $1`, telegramID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return tx.Commit(ctx)
}

func (p *Postgres) Stats(ctx context.Context, telegramID int64, now time.Time, defaultLimit int) (domain.UserStats, error) {
	stats := domain.UserStats{DailyLimit: defaultLimit, Plan: "free"}
	day := now.Format("2006-01-02")
	err := p.pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT sum(used) FROM daily_usage WHERE telegram_id = $1 AND usage_day = $2::date AND category = 'text'), 0),
		       (SELECT count(*) FROM generations WHERE telegram_id = $1)`, telegramID, day).Scan(&stats.UsedToday, &stats.Total)
	if err != nil {
		return stats, err
	}
	var limitsJSON []byte
	err = p.pool.QueryRow(ctx, `SELECT plan, limits FROM entitlements WHERE telegram_id = $1 AND (expires_at IS NULL OR expires_at > $2)`, telegramID, now).Scan(&stats.Plan, &limitsJSON)
	if err == nil {
		var limits map[string]int
		if json.Unmarshal(limitsJSON, &limits) == nil {
			if value := limits[string(domain.QuotaText)]; value > 0 {
				stats.DailyLimit = value
			}
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return stats, err
	}
	return stats, nil
}

func (p *Postgres) DeleteUser(ctx context.Context, telegramID int64) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if _, err := tx.Exec(ctx, `DELETE FROM telegram_update_jobs WHERE actor_id = $1`, telegramID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE telegram_id = $1`, telegramID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (p *Postgres) Cleanup(ctx context.Context, before time.Time) (int64, error) {
	// Recover expired publish claims in a short transaction of their own. It
	// only locks drafts and releases them before retention starts acquiring
	// brief locks, keeping the global editorial lock order brief -> draft.
	if _, err := p.pool.Exec(ctx, `
		UPDATE thread_drafts
		SET state = CASE
				WHEN publish_started_at IS NULL THEN 'failed'
				ELSE 'unknown'
			END,
			error_code = CASE
				WHEN publish_started_at IS NULL THEN 'publish_lease_expired_before_attempt'
				ELSE 'publish_lease_expired'
			END,
			claim_token = '', claim_expires_at = NULL, updated_at = now()
		WHERE state = 'publishing'
		  AND (claim_expires_at IS NULL OR claim_expires_at <= now())`); err != nil {
		return 0, err
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	tag, err := tx.Exec(ctx, `DELETE FROM generations WHERE created_at < $1`, before)
	if err != nil {
		return 0, err
	}
	// Lock retention candidates before the delete statement takes its fresh
	// READ COMMITTED snapshot. A concurrent refinement holds FOR SHARE on the
	// same brief; after it commits, the next statement sees its fresh linked
	// draft and preserves the factual material.
	rows, err := tx.Query(ctx, `
		SELECT id FROM thread_briefs WHERE updated_at < $1 FOR UPDATE`, before)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var briefID int64
		if err := rows.Scan(&briefID); err != nil {
			rows.Close()
			return 0, err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if _, err := tx.Exec(ctx, `
		DELETE FROM thread_briefs AS brief
		WHERE brief.updated_at < $1
		  AND NOT EXISTS (
			SELECT 1 FROM thread_drafts AS draft
			WHERE draft.brief_id = brief.id AND draft.updated_at >= $1
			  )`, before); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM thread_drafts WHERE updated_at < $1`, before); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM thread_media AS media
		WHERE NOT EXISTS (SELECT 1 FROM thread_drafts AS draft WHERE draft.media_id = media.id)`); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM processed_updates WHERE processed_at < $1`, before); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM quota_reservations AS reservation
		WHERE reservation.updated_at < $1
		  AND NOT EXISTS (
			SELECT 1 FROM telegram_update_jobs AS job
			WHERE job.update_id = reservation.reservation_id
			  AND job.actor_id = reservation.telegram_id
			  AND job.status IN ('pending', 'processing')
		  )`, before); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM telegram_update_jobs
		WHERE status IN ('completed', 'dead', 'superseded') AND completed_at < $1`, before); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func mapNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func fallback(value, fallbackValue string) string {
	if value == "" {
		return fallbackValue
	}
	return value
}

func truncateErrorCode(value string) string {
	const maxLength = 160
	if len(value) <= maxLength {
		return value
	}
	return value[:maxLength]
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}

const threadDraftColumns = `
	id, telegram_id, voice, goal, brief_id, objective, scenario_id, generation_id,
	generation_update_id, photo_query, preview_text, provider, model, revision,
	media_mode, media_id, media_rights_confirmed_at,
	state, is_current, container_id, post_id, permalink, error_code,
	claim_token, claim_expires_at, publish_started_at,
	created_at, updated_at, published_at`

const threadDraftSelect = `SELECT ` + threadDraftColumns + ` FROM thread_drafts`

type threadDraftScanner interface {
	Scan(dest ...any) error
}

func scanThreadDraft(row threadDraftScanner) (domain.ThreadDraft, error) {
	var draft domain.ThreadDraft
	var revision int64
	var briefID sql.NullInt64
	var generationUpdateID sql.NullInt64
	var mediaID sql.NullInt64
	err := row.Scan(
		&draft.ID, &draft.TelegramID, &draft.Voice, &draft.Goal, &briefID, &draft.Objective,
		&draft.ScenarioID, &draft.GenerationID, &generationUpdateID, &draft.PhotoQuery, &draft.Text,
		&draft.Provider, &draft.Model, &revision, &draft.MediaMode, &mediaID,
		&draft.MediaRightsConfirmedAt, &draft.State, &draft.Current,
		&draft.ContainerID, &draft.PostID, &draft.Permalink, &draft.ErrorCode,
		&draft.ClaimToken, &draft.ClaimExpiresAt, &draft.PublishStartedAt,
		&draft.CreatedAt, &draft.UpdatedAt, &draft.PublishedAt,
	)
	if err == nil {
		if revision < 1 || uint64(revision) > uint64(^uint32(0)) {
			return domain.ThreadDraft{}, fmt.Errorf("invalid persisted thread draft revision %d", revision)
		}
		draft.Revision = uint32(revision)
		if briefID.Valid {
			draft.BriefID = briefID.Int64
		}
		if generationUpdateID.Valid {
			draft.GenerationUpdateID = generationUpdateID.Int64
		}
		if mediaID.Valid {
			draft.MediaID = mediaID.Int64
		}
	}
	return draft, err
}

const threadBriefColumns = `
	id, telegram_id, start_update_id, voice, objective, material_kind,
	material_text, material_update_id, state, revision, is_current, error_code,
	created_at, updated_at`

const threadBriefSelect = `SELECT ` + threadBriefColumns + ` FROM thread_briefs`

func scanThreadBrief(row threadDraftScanner) (domain.ThreadBrief, error) {
	var brief domain.ThreadBrief
	var materialUpdateID sql.NullInt64
	var revision int64
	err := row.Scan(
		&brief.ID, &brief.TelegramID, &brief.StartUpdateID, &brief.Voice, &brief.Objective,
		&brief.MaterialKind, &brief.MaterialText, &materialUpdateID, &brief.State,
		&revision, &brief.Current, &brief.ErrorCode, &brief.CreatedAt, &brief.UpdatedAt,
	)
	if err != nil {
		return domain.ThreadBrief{}, err
	}
	if revision < 1 || uint64(revision) > uint64(^uint32(0)) {
		return domain.ThreadBrief{}, fmt.Errorf("invalid persisted thread brief revision %d", revision)
	}
	brief.Revision = uint32(revision)
	if materialUpdateID.Valid {
		brief.MaterialUpdateID = materialUpdateID.Int64
	}
	if err := brief.Validate(); err != nil {
		return domain.ThreadBrief{}, fmt.Errorf("invalid persisted thread brief: %w", err)
	}
	return brief, nil
}

const threadMediaColumns = `
	id, telegram_id, source_kind, source_update_id, attach_update_id, source_asset_id,
	source_page_url, source_author, source_author_url, source_query,
	content, media_type, width, height, digest, delivery_key, created_at`

const threadMediaSelect = `SELECT ` + threadMediaColumns + ` FROM thread_media`

func scanThreadMedia(row threadDraftScanner) (domain.ThreadMedia, error) {
	var mediaValue domain.ThreadMedia
	var sourceUpdateID sql.NullInt64
	var attachUpdateID sql.NullInt64
	err := row.Scan(
		&mediaValue.ID, &mediaValue.TelegramID, &mediaValue.SourceKind, &sourceUpdateID, &attachUpdateID,
		&mediaValue.SourceAssetID, &mediaValue.SourcePageURL, &mediaValue.SourceAuthor,
		&mediaValue.SourceAuthorURL, &mediaValue.SourceQuery,
		&mediaValue.Data, &mediaValue.MediaType, &mediaValue.Width, &mediaValue.Height,
		&mediaValue.Digest, &mediaValue.DeliveryKey, &mediaValue.CreatedAt,
	)
	if err == nil {
		if sourceUpdateID.Valid {
			mediaValue.SourceUpdateID = sourceUpdateID.Int64
		}
		if attachUpdateID.Valid {
			mediaValue.AttachUpdateID = attachUpdateID.Int64
		}
		err = mediaValue.ValidateForStore()
	}
	return mediaValue, err
}

func nullablePositiveInt64(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}
