-- Trigger posts can now be saved automatically from new Page posts.
ALTER TABLE trigger_posts
    ADD COLUMN IF NOT EXISTS post_message TEXT,                              -- post text when saved
    ADD COLUMN IF NOT EXISTS source       TEXT NOT NULL DEFAULT 'manual',    -- 'manual' | 'auto'
    ADD COLUMN IF NOT EXISTS updated_at   TIMESTAMPTZ NOT NULL DEFAULT now();
