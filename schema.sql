-- ============================================================
-- fb-order-bot schema
-- ============================================================

CREATE TABLE pages (
    id                  BIGSERIAL PRIMARY KEY,
    fb_page_id          TEXT NOT NULL UNIQUE,
    name                TEXT NOT NULL,
    page_access_token   TEXT NOT NULL,      -- store encrypted at rest in real deployments
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE products (
    id                  BIGSERIAL PRIMARY KEY,
    page_id             BIGINT NOT NULL REFERENCES pages(id),
    name                TEXT NOT NULL,
    description         TEXT,
    price_cents         INTEGER NOT NULL,
    currency            TEXT NOT NULL DEFAULT 'sgd',
    image_url           TEXT,
    active              BOOLEAN NOT NULL DEFAULT true,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per "trigger" — the comment that kicked off an order conversation.
-- psid = page-scoped id of the commenter, resolved once we message them.
CREATE TABLE orders (
    id                          BIGSERIAL PRIMARY KEY,
    page_id                     BIGINT NOT NULL REFERENCES pages(id),
    product_id                  BIGINT REFERENCES products(id),
    fb_comment_id               TEXT NOT NULL,
    commenter_fb_id             TEXT,                -- comment "from.id" (page-scoped, not the PSID)
    commenter_name              TEXT,
    comment_message             TEXT,                -- the comment that triggered the order
    post_permalink              TEXT,
    psid                        TEXT,                -- filled in once the user replies in Messenger
    quantity                    INTEGER NOT NULL DEFAULT 1,
    amount_cents                INTEGER,
    currency                    TEXT NOT NULL DEFAULT 'sgd',
    customer_email              TEXT,
    payment_method              TEXT,                -- 'card' | 'paynow'
    status                      TEXT NOT NULL DEFAULT 'pending_form',
        -- pending_form -> pending_payment -> paid -> fulfilled | payment_failed | cancelled
    stripe_checkout_session_id  TEXT,
    stripe_payment_intent_id    TEXT,
    last_error                  TEXT,                -- most recent failure, cleared on success
    last_error_step             TEXT,                -- e.g. 'private_reply', 'confirmation_email'
    last_error_at               TIMESTAMPTZ,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_orders_page_status ON orders(page_id, status);
CREATE UNIQUE INDEX idx_orders_comment ON orders(fb_comment_id);
CREATE INDEX idx_orders_failed ON orders(last_error_at) WHERE last_error IS NOT NULL;

-- Dedupe table for inbound webhooks (Meta and Stripe both redeliver).
CREATE TABLE webhook_events (
    id            BIGSERIAL PRIMARY KEY,
    source        TEXT NOT NULL,        -- 'facebook' | 'stripe'
    event_id      TEXT NOT NULL,        -- FB: comment_id+edit ts, Stripe: evt_id
    received_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source, event_id)
);
