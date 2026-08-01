-- Version 4 introduces a durable pre-generation brief for Belcanto Threads.
-- A brief may be incomplete; thread_drafts remain exact publishable previews.
CREATE TABLE IF NOT EXISTS thread_briefs (
    id BIGSERIAL PRIMARY KEY,
    telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    start_update_id BIGINT NOT NULL CHECK (start_update_id > 0),
    voice TEXT NOT NULL CHECK (voice IN ('belcanto', 'alisher')),
    objective TEXT NOT NULL DEFAULT ''
        CHECK (objective IN ('', 'reach', 'replies', 'trust', 'trial', 'community')),
    material_kind TEXT NOT NULL DEFAULT ''
        CHECK (material_kind IN ('', 'none', 'text')),
    material_text TEXT NOT NULL DEFAULT ''
        CHECK (char_length(material_text) <= 6000),
    material_update_id BIGINT CHECK (material_update_id > 0),
    state TEXT NOT NULL DEFAULT 'awaiting_goal'
        CHECK (state IN ('awaiting_goal', 'awaiting_material', 'material_ready', 'draft_ready', 'cancelled')),
    revision BIGINT NOT NULL DEFAULT 1
        CHECK (revision > 0 AND revision <= 4294967295),
    is_current BOOLEAN NOT NULL DEFAULT TRUE,
    error_code TEXT NOT NULL DEFAULT '' CHECK (char_length(error_code) <= 160),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT thread_briefs_state_invariant_check CHECK (
        (state = 'awaiting_goal'
            AND objective = '' AND material_kind = '' AND material_text = ''
            AND material_update_id IS NULL)
        OR
        (state = 'awaiting_material'
            AND objective IN ('reach', 'replies', 'trust', 'trial', 'community')
            AND material_kind = '' AND material_text = '' AND material_update_id IS NULL)
        OR
        (state IN ('material_ready', 'draft_ready')
            AND objective IN ('reach', 'replies', 'trust', 'trial', 'community')
            AND material_update_id IS NOT NULL
            AND (
                (material_kind = 'none' AND material_text = '')
                OR (material_kind = 'text' AND char_length(btrim(material_text)) > 0)
            ))
        OR
        (state = 'cancelled' AND NOT is_current
            AND (
                (objective = '' AND material_kind = '' AND material_text = '' AND material_update_id IS NULL)
                OR
                (objective IN ('reach', 'replies', 'trust', 'trial', 'community') AND (
                    (material_kind = '' AND material_text = '' AND material_update_id IS NULL)
                    OR (material_kind = 'none' AND material_text = '' AND material_update_id IS NOT NULL)
                    OR (material_kind = 'text' AND char_length(btrim(material_text)) > 0 AND material_update_id IS NOT NULL)
                ))
            ))
    ),
    UNIQUE (telegram_id, start_update_id),
    UNIQUE (telegram_id, material_update_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_thread_briefs_one_current_per_user
    ON thread_briefs (telegram_id)
    WHERE is_current;

CREATE INDEX IF NOT EXISTS idx_thread_briefs_user_updated
    ON thread_briefs (telegram_id, updated_at DESC, id DESC);

ALTER TABLE thread_drafts
    ADD COLUMN IF NOT EXISTS brief_id BIGINT REFERENCES thread_briefs(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS objective TEXT NOT NULL DEFAULT 'legacy',
    ADD COLUMN IF NOT EXISTS scenario_id TEXT NOT NULL DEFAULT 'legacy_unspecified',
    ADD COLUMN IF NOT EXISTS generation_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS generation_update_id BIGINT;

ALTER TABLE thread_drafts DROP CONSTRAINT IF EXISTS thread_drafts_objective_check;
ALTER TABLE thread_drafts ADD CONSTRAINT thread_drafts_objective_check
    CHECK (objective IN ('reach', 'replies', 'trust', 'trial', 'community', 'legacy'));

ALTER TABLE thread_drafts DROP CONSTRAINT IF EXISTS thread_drafts_scenario_id_check;
ALTER TABLE thread_drafts ADD CONSTRAINT thread_drafts_scenario_id_check
    CHECK (scenario_id ~ '^[a-z][a-z0-9_]{2,63}$');

ALTER TABLE thread_drafts DROP CONSTRAINT IF EXISTS thread_drafts_generation_id_length_check;
ALTER TABLE thread_drafts ADD CONSTRAINT thread_drafts_generation_id_length_check
    CHECK (char_length(generation_id) <= 64);

ALTER TABLE thread_drafts DROP CONSTRAINT IF EXISTS thread_drafts_generation_update_id_check;
ALTER TABLE thread_drafts ADD CONSTRAINT thread_drafts_generation_update_id_check
    CHECK (generation_update_id IS NULL OR generation_update_id > 0);

ALTER TABLE thread_drafts DROP CONSTRAINT IF EXISTS thread_drafts_brief_metadata_check;
ALTER TABLE thread_drafts ADD CONSTRAINT thread_drafts_brief_metadata_check CHECK (
    brief_id IS NULL
    OR (
        objective IN ('reach', 'replies', 'trust', 'trial', 'community')
        AND scenario_id <> 'legacy_unspecified'
        AND char_length(btrim(generation_id)) > 0
        AND generation_update_id IS NOT NULL
    )
);

CREATE INDEX IF NOT EXISTS idx_thread_drafts_brief
    ON thread_drafts (brief_id, created_at DESC, id DESC)
    WHERE brief_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_thread_drafts_generation_update
    ON thread_drafts (telegram_id, generation_update_id)
    WHERE generation_update_id IS NOT NULL;

INSERT INTO schema_migrations (version) VALUES (4) ON CONFLICT (version) DO NOTHING;
