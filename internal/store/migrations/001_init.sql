CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES (1) ON CONFLICT (version) DO NOTHING;

CREATE TABLE IF NOT EXISTS users (
    telegram_id BIGINT PRIMARY KEY,
    language_code TEXT NOT NULL DEFAULT 'ru',
    default_tone TEXT NOT NULL DEFAULT 'mix',
    consented_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS processed_updates (
    update_id BIGINT PRIMARY KEY,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS telegram_update_jobs (
    update_id BIGINT PRIMARY KEY,
    actor_id BIGINT NOT NULL,
    payload BYTEA NOT NULL,
    supersedable BOOLEAN NOT NULL DEFAULT FALSE,
    superseding BOOLEAN NOT NULL DEFAULT FALSE,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'completed', 'dead', 'superseded')),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_until TIMESTAMPTZ,
    lease_token TEXT,
    last_error TEXT NOT NULL DEFAULT '',
    enqueued_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    CHECK (
        (status = 'processing' AND lease_until IS NOT NULL AND lease_token IS NOT NULL)
        OR (status <> 'processing' AND lease_until IS NULL AND lease_token IS NULL)
    )
);

-- Keep the bootstrap migration upgrade-safe for installations that ran an
-- earlier development version before durable supersession was introduced.
ALTER TABLE telegram_update_jobs
    ADD COLUMN IF NOT EXISTS supersedable BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS superseding BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE telegram_update_jobs
    DROP CONSTRAINT IF EXISTS telegram_update_jobs_status_check;
ALTER TABLE telegram_update_jobs
    ADD CONSTRAINT telegram_update_jobs_status_check
    CHECK (status IN ('pending', 'processing', 'completed', 'dead', 'superseded'));

CREATE INDEX IF NOT EXISTS idx_telegram_update_jobs_claim
    ON telegram_update_jobs (update_id)
    WHERE status IN ('pending', 'processing');

CREATE INDEX IF NOT EXISTS idx_telegram_update_jobs_actor
    ON telegram_update_jobs (actor_id, update_id)
    WHERE status IN ('pending', 'processing');

-- Preserve idempotency across upgrades from the original processed-update
-- tombstone. These rows contain no recoverable payload and are never claimed.
INSERT INTO telegram_update_jobs (update_id, actor_id, payload, status, completed_at)
SELECT update_id, 0, ''::bytea, 'completed', processed_at
FROM processed_updates
ON CONFLICT (update_id) DO NOTHING;

CREATE TABLE IF NOT EXISTS entitlements (
    telegram_id BIGINT PRIMARY KEY REFERENCES users(telegram_id) ON DELETE CASCADE,
    plan TEXT NOT NULL DEFAULT 'free',
    limits JSONB NOT NULL DEFAULT '{}'::jsonb,
    expires_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS daily_usage (
    telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    usage_day DATE NOT NULL,
    category TEXT NOT NULL,
    used INTEGER NOT NULL DEFAULT 0 CHECK (used >= 0),
    PRIMARY KEY (telegram_id, usage_day, category)
);

CREATE TABLE IF NOT EXISTS quota_reservations (
    telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    reservation_id BIGINT NOT NULL CHECK (reservation_id > 0),
    category TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('charged', 'refunded', 'denied')),
    usage_day DATE NOT NULL,
    used_snapshot INTEGER NOT NULL CHECK (used_snapshot >= 0),
    limit_snapshot INTEGER NOT NULL CHECK (limit_snapshot > 0),
    resets_at TIMESTAMPTZ NOT NULL,
    plan_snapshot TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (telegram_id, reservation_id, category)
);

CREATE INDEX IF NOT EXISTS idx_quota_reservations_updated
    ON quota_reservations (updated_at);

CREATE TABLE IF NOT EXISTS generations (
    id BIGSERIAL PRIMARY KEY,
    telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    input_kind TEXT NOT NULL,
    input_digest TEXT NOT NULL,
    tone TEXT NOT NULL,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    result JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_generations_user_created ON generations (telegram_id, created_at DESC);

CREATE TABLE IF NOT EXISTS feedback (
    id BIGSERIAL PRIMARY KEY,
    generation_id BIGINT NOT NULL REFERENCES generations(id) ON DELETE CASCADE,
    telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    candidate SMALLINT NOT NULL DEFAULT -1,
    rating SMALLINT NOT NULL DEFAULT 0 CHECK (rating BETWEEN -1 AND 1),
    action TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (generation_id, telegram_id, candidate, action)
);

CREATE TABLE IF NOT EXISTS style_examples (
    id BIGSERIAL PRIMARY KEY,
    telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    source_generation_id BIGINT REFERENCES generations(id) ON DELETE SET NULL,
    example_text TEXT NOT NULL CHECK (char_length(example_text) BETWEEN 1 AND 500),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_style_examples_user_created ON style_examples (telegram_id, created_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS idx_style_examples_unique_text
    ON style_examples (telegram_id, example_text);
