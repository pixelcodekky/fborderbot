-- For databases created before last_error tracking was added to schema.sql.
ALTER TABLE orders
    ADD COLUMN IF NOT EXISTS last_error      TEXT,
    ADD COLUMN IF NOT EXISTS last_error_step TEXT,
    ADD COLUMN IF NOT EXISTS last_error_at   TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_orders_failed ON orders(last_error_at) WHERE last_error IS NOT NULL;
