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
LOG_LEVEL                 optional: debug | info (default) | warn | error
SENTRY_DSN                optional: enables error alerts (see Alerting)
SENTRY_ENVIRONMENT        optional: defaults to "production"
```
The app exits at startup if any of the required ones are missing.

## Debugging in production
Logs are JSON on stdout, one line per event. Every line from a webhook carries
`request_id` and, once known, `source`, `comment_id`, `page_id`, `order_id`,
`psid`, `stripe_event_id` and `step`, so filtering on `order_id` shows an
order's whole history.

Failures are also recorded on the order itself:
```sql
SELECT id, status, last_error_step, last_error, last_error_at
FROM orders WHERE last_error IS NOT NULL ORDER BY last_error_at DESC;
```

Retries: transient failures (DB errors, network errors, Graph API 5xx) release
the webhook dedupe claim and return 500, so Meta/Stripe redeliver and the event
is processed again. Permanent ones (unknown page, Graph API 4xx, bad metadata)
are logged and ACKed.

`/health` pings the database and returns 503 if it is unreachable.

## Alerting
**Sentry (errors).** Create a Go project at sentry.io (free tier is enough) and
set `SENTRY_DSN`. Every ERROR log line is then also sent to Sentry:
- the issue title is `<message>: <error>`, e.g. `private_reply failed: messenger API error (400) ...`
- issues are grouped by log message, so one failing step = one issue
- `order_id`, `page_id`, `comment_id`, `stripe_event_id`, `source`, `step`
  and `request_id` are tags you can search on; the `request_id` finds the full
  log trail in Render
- the same message is sent at most once a minute (logs still have every
  occurrence), so an outage doesn't use up the quota

In Sentry, add an alert rule "a new issue is created" → email/Slack. On Render,
`RENDER_GIT_COMMIT` is sent as the release, so you can see which deploy
introduced an issue. WARN lines (bad webhook signatures, unknown checkout
sessions) are not sent: scanners hitting the webhook URLs would be noisy.

**Uptime.** Point an external monitor (UptimeRobot, Better Stack) at
`https://<your-app>/health` every 1–5 minutes with email/Slack alerts. Also set
Render's health check path to `/health` so a dead DB connection restarts the
service.

**Provider dashboards.** Failed deliveries show up on the sender's side too:
Stripe → Developers → Webhooks → your endpoint (per-event responses, resend
button), and Meta App dashboard → Webhooks. Meta may disable a webhook
subscription that keeps failing, so check there if comments stop arriving.

## Migrations
`schema.sql` is the full schema for a new database. For an existing one, apply
the files in `migrations/` in order.

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
