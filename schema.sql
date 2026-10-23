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

-- Keywords per page. A new Page post containing one becomes a trigger post.
CREATE TABLE trigger_keywords (
    id          BIGSERIAL PRIMARY KEY,
    page_id     BIGINT NOT NULL REFERENCES pages(id),
    keyword     TEXT NOT NULL,
    product_id  BIGINT REFERENCES products(id),  -- what the keyword sells; price for the reply
    active      BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_trigger_keywords_page_keyword ON trigger_keywords(page_id, lower(keyword));

-- Only comments on posts listed here can start an order. One row per keyword
-- found in the post; a comment must contain one of the post's keywords.
CREATE TABLE trigger_posts (
    id            BIGSERIAL PRIMARY KEY,
    page_id       BIGINT NOT NULL REFERENCES pages(id),
    fb_post_id    TEXT NOT NULL,      -- as in the webhook's post_id: "<page id>_<post id>"
    keyword       TEXT,               -- NULL (manual rows) = the default trigger keyword
    active        BOOLEAN NOT NULL DEFAULT true,
    post_message  TEXT,               -- post text when saved
    source        TEXT NOT NULL DEFAULT 'manual',  -- 'manual' | 'auto' (from a new Page post)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- NULLS NOT DISTINCT needs Postgres 15+.
CREATE UNIQUE INDEX idx_trigger_posts_page_post_keyword
    ON trigger_posts(page_id, fb_post_id, keyword) NULLS NOT DISTINCT;

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
    quantity                    INTEGER NOT NULL DEFAULT 1,     -- chosen by the customer, 1-10
    unit_price_cents            INTEGER,             -- product price when the order was made
    amount_cents                INTEGER,             -- unit_price_cents x quantity; what payment pages charge
    currency                    TEXT NOT NULL DEFAULT 'sgd',
    customer_email              TEXT,
    payment_method              TEXT,                -- '2c2p' | 'paynow' | 'stripe' (card)
    status                      TEXT NOT NULL DEFAULT 'pending_form',
        -- pending_form -> pending_payment -> paid -> fulfilled | payment_failed | cancelled
    stripe_checkout_session_id  TEXT,
    stripe_payment_intent_id    TEXT,
    receipt_token               TEXT,                -- set when paid; the receipt is at /receipts/<token>
    last_error                  TEXT,                -- most recent failure, cleared on success
    last_error_step             TEXT,                -- e.g. 'private_reply', 'confirmation_email'
    last_error_at               TIMESTAMPTZ,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_orders_page_status ON orders(page_id, status);
CREATE UNIQUE INDEX idx_orders_comment ON orders(fb_comment_id);
CREATE INDEX idx_orders_failed ON orders(last_error_at) WHERE last_error IS NOT NULL;
CREATE UNIQUE INDEX idx_orders_receipt_token ON orders(receipt_token);

-- One row per payment page (2C2P or PayNow) made for an order. An order has at
-- most one pending attempt across methods, so it never has two pages that can
-- both take its money.
CREATE TABLE payment_attempts (
    id            BIGSERIAL PRIMARY KEY,
    order_id      BIGINT NOT NULL REFERENCES orders(id),
    provider      TEXT NOT NULL DEFAULT '2c2p',  -- payment method: '2c2p' | 'paynow' | 'stripe' (both Stripe Checkout)
    invoice_no    TEXT NOT NULL UNIQUE,      -- our reference, e.g. FB42T1792459910123R9F3A (Stripe: client_reference_id)
    provider_ref  TEXT,                      -- the provider's id for the page (Stripe Checkout Session ID)
    amount_cents  INTEGER NOT NULL,          -- what this page charges; results are checked against it
    currency      TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending',  -- pending -> succeeded | failed
    payment_url   TEXT,                      -- NULL while the page is being created
    expires_at    TIMESTAMPTZ NOT NULL,      -- after it, the page takes no payment (2C2P paymentExpiry, Stripe expires_at)
    retry_of      BIGINT UNIQUE REFERENCES payment_attempts(id),  -- the failed attempt this replaces (once)
    resp_code     TEXT,
    resp_desc     TEXT,
    tran_ref      TEXT,                      -- 2C2P tranRef / Stripe PaymentIntent ID
    channel_code  TEXT,                      -- e.g. 'VI', 'paynow'
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_payment_attempts_one_pending
    ON payment_attempts(order_id) WHERE status = 'pending';
CREATE INDEX idx_payment_attempts_order ON payment_attempts(order_id);

-- Dedupe table for inbound webhooks (Meta and Stripe both redeliver).
CREATE TABLE webhook_events (
    id            BIGSERIAL PRIMARY KEY,
    source        TEXT NOT NULL,        -- 'facebook' | 'stripe' | '2c2p'
    event_id      TEXT NOT NULL,        -- FB: comment_id+edit ts, Stripe: evt_id
    received_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source, event_id)
);
