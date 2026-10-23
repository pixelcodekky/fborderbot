-- The customer picks a quantity before paying: amount_cents becomes
-- unit_price_cents x quantity. The unit price is kept from when the order was
-- made, so a later product price change doesn't move an open order.
ALTER TABLE orders ADD COLUMN IF NOT EXISTS unit_price_cents INTEGER;
UPDATE orders SET unit_price_cents = amount_cents / quantity
 WHERE unit_price_cents IS NULL AND amount_cents IS NOT NULL AND quantity > 0;
