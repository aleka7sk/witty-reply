-- Version 5 keeps all five reviewed Threads finalists durable until the
-- operator chooses one. Only that atomic choice creates a publishable draft.

ALTER TABLE thread_briefs DROP CONSTRAINT IF EXISTS thread_briefs_state_check;
ALTER TABLE thread_briefs ADD CONSTRAINT thread_briefs_state_check
    CHECK (state IN (
        'awaiting_goal', 'awaiting_material', 'material_ready',
        'candidates_ready', 'draft_ready', 'cancelled'
    ));

ALTER TABLE thread_briefs DROP CONSTRAINT IF EXISTS thread_briefs_state_invariant_check;
ALTER TABLE thread_briefs ADD CONSTRAINT thread_briefs_state_invariant_check CHECK (
    (state = 'awaiting_goal'
        AND objective = '' AND material_kind = '' AND material_text = ''
        AND material_update_id IS NULL)
    OR
    (state = 'awaiting_material'
        AND objective IN ('reach', 'replies', 'trust', 'trial', 'community')
        AND material_kind = '' AND material_text = '' AND material_update_id IS NULL)
    OR
    (state IN ('material_ready', 'candidates_ready', 'draft_ready')
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
);

CREATE TABLE IF NOT EXISTS thread_finalist_sets (
    id BIGSERIAL PRIMARY KEY,
    telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    brief_id BIGINT REFERENCES thread_briefs(id) ON DELETE CASCADE,
    base_draft_id BIGINT REFERENCES thread_drafts(id),
    generation_id TEXT NOT NULL
        CHECK (char_length(btrim(generation_id)) BETWEEN 1 AND 64),
    generation_update_id BIGINT NOT NULL CHECK (generation_update_id > 0),
    voice TEXT NOT NULL CHECK (voice IN ('belcanto', 'alisher')),
    objective TEXT NOT NULL
        CHECK (objective IN ('reach', 'replies', 'trust', 'trial', 'community')),
    provider TEXT NOT NULL CHECK (char_length(btrim(provider)) BETWEEN 1 AND 100),
    model TEXT NOT NULL CHECK (char_length(btrim(model)) BETWEEN 1 AND 160),
    source_revision BIGINT NOT NULL
        CHECK (source_revision > 0 AND source_revision <= 4294967295),
    target_draft_revision BIGINT NOT NULL
        CHECK (target_draft_revision > 0 AND target_draft_revision <= 4294967295),
    preserve_media_mode TEXT NOT NULL DEFAULT ''
        CHECK (preserve_media_mode IN ('', 'image_pending', 'image')),
    preserve_media_id BIGINT REFERENCES thread_media(id),
    state TEXT NOT NULL DEFAULT 'ready'
        CHECK (state IN ('ready', 'selected', 'cancelled')),
    revision BIGINT NOT NULL DEFAULT 1
        CHECK (revision > 0 AND revision <= 4294967295),
    is_current BOOLEAN NOT NULL DEFAULT TRUE,
    selected_position SMALLINT CHECK (selected_position BETWEEN 0 AND 4),
    selection_update_id BIGINT CHECK (selection_update_id > 0),
    selected_draft_id BIGINT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT thread_finalist_sets_source_check CHECK (
        brief_id IS NOT NULL OR base_draft_id IS NOT NULL
    ),
    CONSTRAINT thread_finalist_sets_preserve_media_check CHECK (
        (preserve_media_mode = '' AND preserve_media_id IS NULL)
        OR preserve_media_mode = 'image_pending'
        OR (preserve_media_mode = 'image' AND preserve_media_id IS NOT NULL)
    ),
    CONSTRAINT thread_finalist_sets_lifecycle_check CHECK (
        (state = 'ready' AND is_current
            AND selected_position IS NULL
            AND selection_update_id IS NULL
            AND selected_draft_id IS NULL)
        OR
        (state = 'selected' AND NOT is_current
            AND selected_position IS NOT NULL
            AND selection_update_id IS NOT NULL
            AND selected_draft_id IS NOT NULL)
        OR
        (state = 'cancelled' AND NOT is_current
            AND selected_position IS NULL
            AND selection_update_id IS NULL
            AND selected_draft_id IS NULL)
    ),
    UNIQUE (telegram_id, generation_update_id),
    UNIQUE (telegram_id, generation_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_thread_finalist_sets_one_current_per_user
    ON thread_finalist_sets (telegram_id)
    WHERE is_current;

CREATE UNIQUE INDEX IF NOT EXISTS idx_thread_finalist_sets_selection_update
    ON thread_finalist_sets (telegram_id, selection_update_id)
    WHERE selection_update_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_thread_finalist_sets_brief
    ON thread_finalist_sets (brief_id, created_at DESC, id DESC)
    WHERE brief_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS thread_finalists (
    set_id BIGINT NOT NULL REFERENCES thread_finalist_sets(id) ON DELETE CASCADE,
    position SMALLINT NOT NULL CHECK (position BETWEEN 0 AND 4),
    reviewer_id TEXT NOT NULL CHECK (char_length(btrim(reviewer_id)) BETWEEN 1 AND 16),
    goal TEXT NOT NULL CHECK (char_length(btrim(goal)) BETWEEN 1 AND 120),
    objective TEXT NOT NULL
        CHECK (objective IN ('reach', 'replies', 'trust', 'trial', 'community')),
    scenario_id TEXT NOT NULL CHECK (scenario_id ~ '^[a-z][a-z0-9_]{2,63}$'),
    mechanism TEXT NOT NULL CHECK (char_length(btrim(mechanism)) BETWEEN 1 AND 120),
    material_basis TEXT NOT NULL CHECK (material_basis IN ('none', 'material')),
    preview_text TEXT NOT NULL CHECK (
        char_length(btrim(preview_text)) >= 1 AND char_length(preview_text) <= 500
    ),
    recommended BOOLEAN NOT NULL DEFAULT FALSE,
    selectable BOOLEAN NOT NULL DEFAULT FALSE,
    visual_mode TEXT NOT NULL CHECK (
        visual_mode IN ('text_only', 'licensed_photo', 'belcanto_photo')
    ),
    photo_suggested BOOLEAN NOT NULL DEFAULT FALSE,
    photo_query TEXT NOT NULL DEFAULT '' CHECK (char_length(photo_query) <= 100),
    PRIMARY KEY (set_id, position),
    UNIQUE (set_id, reviewer_id),
    UNIQUE (set_id, scenario_id),
    CHECK (NOT photo_suggested OR char_length(btrim(photo_query)) >= 3),
    CHECK (
        (visual_mode = 'text_only' AND NOT photo_suggested)
        OR (visual_mode IN ('licensed_photo', 'belcanto_photo') AND photo_suggested)
    )
);

ALTER TABLE thread_drafts
    ADD COLUMN IF NOT EXISTS finalist_set_id BIGINT
        REFERENCES thread_finalist_sets(id) ON DELETE SET NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_thread_drafts_one_per_finalist_set
    ON thread_drafts (finalist_set_id)
    WHERE finalist_set_id IS NOT NULL;

ALTER TABLE thread_finalist_sets
    DROP CONSTRAINT IF EXISTS thread_finalist_sets_selected_draft_fk;
ALTER TABLE thread_finalist_sets
    ADD CONSTRAINT thread_finalist_sets_selected_draft_fk
    FOREIGN KEY (selected_draft_id) REFERENCES thread_drafts(id);

INSERT INTO schema_migrations (version) VALUES (5) ON CONFLICT (version) DO NOTHING;
