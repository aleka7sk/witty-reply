-- Version 2 introduces licensed photo provenance without rerunning the large
-- bootstrap migration or rebuilding indexes on every application restart.
-- Some version-1 deployments predate all media tables, while later version-1
-- deployments already contain Telegram-upload media. Bootstrap the exact
-- pre-provenance media shape here so both histories converge safely.
CREATE TABLE IF NOT EXISTS thread_media (
    id BIGSERIAL PRIMARY KEY,
    telegram_id BIGINT NOT NULL REFERENCES users(telegram_id) ON DELETE CASCADE,
    source_update_id BIGINT NOT NULL CHECK (source_update_id > 0),
    content BYTEA NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 8388608),
    media_type TEXT NOT NULL CHECK (media_type = 'image/jpeg'),
    width INTEGER NOT NULL CHECK (width BETWEEN 1 AND 8000),
    height INTEGER NOT NULL CHECK (height BETWEEN 1 AND 8000),
    digest TEXT NOT NULL CHECK (digest ~ '^[0-9a-f]{64}$'),
    delivery_key TEXT NOT NULL UNIQUE CHECK (delivery_key ~ '^[0-9a-f]{32}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((width::bigint * height::bigint) <= 12000000),
    CHECK (width BETWEEN 320 AND 1440),
    CHECK (GREATEST(width, height) <= LEAST(width, height) * 10)
);

CREATE INDEX IF NOT EXISTS idx_thread_media_user_created
    ON thread_media (telegram_id, created_at DESC, id DESC);

ALTER TABLE thread_drafts
    ADD COLUMN IF NOT EXISTS media_mode TEXT NOT NULL DEFAULT 'text',
    ADD COLUMN IF NOT EXISTS media_id BIGINT REFERENCES thread_media(id),
    ADD COLUMN IF NOT EXISTS media_rights_confirmed_at TIMESTAMPTZ;

ALTER TABLE thread_drafts DROP CONSTRAINT IF EXISTS thread_drafts_media_mode_check;
ALTER TABLE thread_drafts ADD CONSTRAINT thread_drafts_media_mode_check
    CHECK (media_mode IN ('text', 'image_pending', 'image'));
ALTER TABLE thread_drafts DROP CONSTRAINT IF EXISTS thread_drafts_media_invariant_check;
ALTER TABLE thread_drafts ADD CONSTRAINT thread_drafts_media_invariant_check CHECK (
    (media_mode = 'text' AND media_id IS NULL)
    OR media_mode = 'image_pending'
    OR (media_mode = 'image' AND media_id IS NOT NULL)
);

ALTER TABLE thread_media
    ADD COLUMN IF NOT EXISTS source_kind TEXT NOT NULL DEFAULT 'telegram_upload',
    ADD COLUMN IF NOT EXISTS source_asset_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source_page_url TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source_author TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source_author_url TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS source_query TEXT NOT NULL DEFAULT '';

ALTER TABLE thread_media ALTER COLUMN source_update_id DROP NOT NULL;
ALTER TABLE thread_media DROP CONSTRAINT IF EXISTS thread_media_source_update_id_check;

ALTER TABLE thread_media DROP CONSTRAINT IF EXISTS thread_media_source_kind_check;
ALTER TABLE thread_media ADD CONSTRAINT thread_media_source_kind_check
    CHECK (source_kind IN ('telegram_upload', 'pexels'));

ALTER TABLE thread_media DROP CONSTRAINT IF EXISTS thread_media_source_asset_id_length_check;
ALTER TABLE thread_media ADD CONSTRAINT thread_media_source_asset_id_length_check
    CHECK (char_length(source_asset_id) <= 80);
ALTER TABLE thread_media DROP CONSTRAINT IF EXISTS thread_media_source_page_url_length_check;
ALTER TABLE thread_media ADD CONSTRAINT thread_media_source_page_url_length_check
    CHECK (char_length(source_page_url) <= 2048);
ALTER TABLE thread_media DROP CONSTRAINT IF EXISTS thread_media_source_author_length_check;
ALTER TABLE thread_media ADD CONSTRAINT thread_media_source_author_length_check
    CHECK (char_length(source_author) <= 160);
ALTER TABLE thread_media DROP CONSTRAINT IF EXISTS thread_media_source_author_url_length_check;
ALTER TABLE thread_media ADD CONSTRAINT thread_media_source_author_url_length_check
    CHECK (char_length(source_author_url) <= 2048);
ALTER TABLE thread_media DROP CONSTRAINT IF EXISTS thread_media_source_query_length_check;
ALTER TABLE thread_media ADD CONSTRAINT thread_media_source_query_length_check
    CHECK (char_length(source_query) <= 120);

ALTER TABLE thread_media DROP CONSTRAINT IF EXISTS thread_media_source_invariant_check;
ALTER TABLE thread_media ADD CONSTRAINT thread_media_source_invariant_check CHECK (
    (source_kind = 'telegram_upload' AND source_update_id > 0
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

-- Keep the original global unique index. PostgreSQL permits multiple NULL
-- values, so licensed rows do not conflict, and retaining the same arbiter
-- keeps a rolling deployment compatible with the previous upload query.
CREATE UNIQUE INDEX IF NOT EXISTS idx_thread_media_source_update
    ON thread_media (telegram_id, source_update_id);

INSERT INTO schema_migrations (version) VALUES (2) ON CONFLICT (version) DO NOTHING;
