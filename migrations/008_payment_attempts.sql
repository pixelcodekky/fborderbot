-- One row per 2C2P payment page made for an order. An order has at most one
-- pending attempt, so it never has two pages that can both take its money.
CREATE TABLE IF NOT EXISTS payment_attempts (
    id            BIGSERIAL PRIMARY KEY,
    order_id      BIGINT NOT NULL REFERENCES orders(id),
    provider      TEXT NOT NULL DEFAULT '2c2p',
    invoice_no    TEXT NOT NULL UNIQUE,      -- 2C2P never accepts one twice, e.g. FB42T1792459910123R9F3A
    amount_cents  INTEGER NOT NULL,          -- what this page charges; results are checked against it
    currency      TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending',  -- pending -> succeeded | failed
    payment_url   TEXT,                      -- NULL while the page is being created
    expires_at    TIMESTAMPTZ NOT NULL,      -- sent as paymentExpiry; 2C2P rejects later payments
    retry_of      BIGINT UNIQUE REFERENCES payment_attempts(id),  -- the failed attempt this replaces (once)
    resp_code     TEXT,
    resp_desc     TEXT,
    tran_ref      TEXT,
    channel_code  TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_payment_attempts_one_pending
    ON payment_attempts(order_id) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_payment_attempts_order ON payment_attempts(order_id);
