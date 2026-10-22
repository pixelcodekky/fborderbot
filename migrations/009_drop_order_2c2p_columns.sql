-- An early draft of 008 put 2C2P columns on orders. 2C2P results are recorded
-- per attempt in payment_attempts (invoice_no, tran_ref, ...), so drop them
-- where that draft was applied. No-op elsewhere.
ALTER TABLE orders DROP COLUMN IF EXISTS twoc2p_invoice_no;
ALTER TABLE orders DROP COLUMN IF EXISTS twoc2p_tran_ref;
