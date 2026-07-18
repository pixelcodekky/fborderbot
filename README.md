# fb-order-bot

## Layout
- `schema.sql` — Postgres schema (pages, products, orders, webhook_events)
- `internal/fbwebhook` — Facebook webhook: verification handshake, signature check,
  comment-keyword matching, Private Reply trigger
- `internal/messenger` — thin Send API client (private reply, text, product carousel)
- `internal/payments` — Stripe Checkout Session creation (card + PayNow)
- `internal/stripewebhook` — Stripe webhook: signature check, order status update,
  Messenger + email confirmation
- `internal/db` — pgx pool + generic webhook dedupe helper

## Env vars
```
DATABASE_URL              postgres://...
FB_WEBHOOK_VERIFY_TOKEN   any string you also set in the Meta App dashboard
FB_APP_SECRET             from the Meta App dashboard (for X-Hub-Signature-256)
STRIPE_SECRET_KEY         sk_live_... / sk_test_...
STRIPE_WEBHOOK_SECRET     whsec_... (from the Stripe webhook endpoint config)
```

## Not included (next pass)
- The order-form webview (product selection → quantity/address) that the
  carousel's "Order this" button opens, and the handler that reads its
  submission and calls `payments.CreateCheckoutSession`.
- Page owner dashboard (order list/CRUD) — plain REST + auth, no Meta/Stripe
  complexity involved.
- Retry/queueing (SQS or similar) in front of the FB webhook handler for
  burst traffic — right now it processes inline before returning 200.
- Migration off deprecated message tags (`POST_PURCHASE_UPDATE` etc.) to
  Utility Templates for any messaging that happens outside the window opened
  by the private reply.
