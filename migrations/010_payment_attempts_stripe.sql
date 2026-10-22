-- PayNow (Stripe Checkout) attempts share payment_attempts with 2C2P, so an
-- order has one pending payment page across both methods. provider_ref is
-- the provider's id for the page (the Stripe Checkout Session ID).
ALTER TABLE payment_attempts ADD COLUMN IF NOT EXISTS provider_ref TEXT;
