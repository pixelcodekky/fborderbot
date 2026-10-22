-- Trigger keywords per page. A new Page post whose text contains one of these
-- becomes a trigger post, one trigger_posts row per keyword found.
CREATE TABLE IF NOT EXISTS trigger_keywords (
    id          BIGSERIAL PRIMARY KEY,
    page_id     BIGINT NOT NULL REFERENCES pages(id),
    keyword     TEXT NOT NULL,
    active      BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_trigger_keywords_page_keyword
    ON trigger_keywords(page_id, lower(keyword));

-- Keep the old behaviour: every existing page triggers on "test sales".
INSERT INTO trigger_keywords (page_id, keyword)
SELECT id, 'test sales' FROM pages
ON CONFLICT DO NOTHING;

-- A post can now carry several keywords: unique per (page, post, keyword).
-- NULLS NOT DISTINCT (Postgres 15+) keeps manual rows with no keyword unique too.
ALTER TABLE trigger_posts DROP CONSTRAINT IF EXISTS trigger_posts_page_id_fb_post_id_key;
CREATE UNIQUE INDEX IF NOT EXISTS idx_trigger_posts_page_post_keyword
    ON trigger_posts(page_id, fb_post_id, keyword) NULLS NOT DISTINCT;
