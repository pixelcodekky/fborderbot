-- Only comments on posts listed here can start an order.
CREATE TABLE IF NOT EXISTS trigger_posts (
    id          BIGSERIAL PRIMARY KEY,
    page_id     BIGINT NOT NULL REFERENCES pages(id),
    fb_post_id  TEXT NOT NULL,        -- as in the webhook's post_id: "<page id>_<post id>"
    keyword     TEXT,                 -- overrides the default trigger keyword when set
    active      BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (page_id, fb_post_id)
);
