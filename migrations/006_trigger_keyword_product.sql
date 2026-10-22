-- Each trigger keyword sells one product: its price is shown in the comment
-- reply and copied onto the order.
ALTER TABLE trigger_keywords
    ADD COLUMN IF NOT EXISTS product_id BIGINT REFERENCES products(id);
