-- Version 3 makes the editor's manual Pexels override durable and replay-safe.
-- Existing drafts remain valid with an empty fallback query; all newly
-- generated drafts persist the reviewer's anonymous English query.
ALTER TABLE thread_drafts
    ADD COLUMN IF NOT EXISTS photo_query TEXT NOT NULL DEFAULT '';

ALTER TABLE thread_drafts DROP CONSTRAINT IF EXISTS thread_drafts_photo_query_length_check;
ALTER TABLE thread_drafts ADD CONSTRAINT thread_drafts_photo_query_length_check
    CHECK (char_length(photo_query) <= 100);

ALTER TABLE thread_media
    ADD COLUMN IF NOT EXISTS attach_update_id BIGINT;

ALTER TABLE thread_media DROP CONSTRAINT IF EXISTS thread_media_attach_update_id_check;
ALTER TABLE thread_media ADD CONSTRAINT thread_media_attach_update_id_check
    CHECK (attach_update_id IS NULL OR attach_update_id > 0);

-- One Telegram callback may select at most one durable licensed asset for one
-- owner. Automatic generation-time suggestions keep this value NULL.
CREATE UNIQUE INDEX IF NOT EXISTS idx_thread_media_attach_update
    ON thread_media (telegram_id, attach_update_id)
    WHERE attach_update_id IS NOT NULL;

-- Keep a small idempotency tombstone after an older photo is replaced and its
-- bytes are deleted. This prevents a delayed callback from moving its operation
-- to another draft while allowing immediate post-commit preview replay.
CREATE TABLE IF NOT EXISTS thread_media_attach_operations (
    telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    attach_update_id BIGINT NOT NULL CHECK (attach_update_id > 0),
    draft_id BIGINT NOT NULL REFERENCES thread_drafts(id) ON DELETE CASCADE,
    media_id BIGINT REFERENCES thread_media(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (telegram_id, attach_update_id)
);

CREATE INDEX IF NOT EXISTS idx_thread_media_attach_operations_draft
    ON thread_media_attach_operations (draft_id);

ALTER TABLE thread_media DROP CONSTRAINT IF EXISTS thread_media_source_invariant_check;
ALTER TABLE thread_media ADD CONSTRAINT thread_media_source_invariant_check CHECK (
    (source_kind = 'telegram_upload' AND source_update_id > 0
        AND attach_update_id IS NULL
        AND source_asset_id = '' AND source_page_url = '' AND source_author = ''
        AND source_author_url = '' AND source_query = '')
    OR
    (source_kind = 'pexels' AND source_update_id IS NULL
        AND char_length(btrim(source_asset_id)) > 0
        AND char_length(btrim(source_page_url)) > 0
        AND char_length(btrim(source_author)) > 0
        AND char_length(btrim(source_author_url)) > 0
        AND char_length(btrim(source_query)) > 0)
);

INSERT INTO schema_migrations (version) VALUES (3) ON CONFLICT (version) DO NOTHING;
