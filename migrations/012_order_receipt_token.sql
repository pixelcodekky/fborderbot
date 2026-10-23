-- A paid order's receipt is at /receipts/<receipt_token>. The token is random
-- (the link is the only key to the receipt) and set when the order is paid;
-- orders already paid get one here.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS receipt_token TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_receipt_token ON orders(receipt_token);
UPDATE orders SET receipt_token = replace(gen_random_uuid()::text, '-', '')
 WHERE status = 'paid' AND receipt_token IS NULL;
