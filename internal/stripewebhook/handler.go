package stripewebhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v78"
	"github.com/stripe/stripe-go/v78/webhook"

	"fb-order-bot/internal/db"
	"fb-order-bot/internal/notify"
	"fb-order-bot/internal/obs"
	"fb-order-bot/internal/payments"
)

type Handler struct {
	Pool          *pgxpool.Pool
	WebhookSecret string
	Payments      *payments.Service
	Notify        *notify.Customer
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Detached from the request so a client hang-up can't abort half-done work.
	ctx := context.WithoutCancel(r.Context())
	log := obs.Logger(ctx).With("source", "stripe")

	const maxBodyBytes = int64(65536)
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		log.Warn("read body failed", "err", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	event, err := webhook.ConstructEvent(payload, r.Header.Get("Stripe-Signature"), h.WebhookSecret)
	if err != nil {
		log.Warn("signature verification failed; check STRIPE_WEBHOOK_SECRET", "err", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	log = log.With("stripe_event_id", event.ID, "event_type", string(event.Type))

	// Dedupe on Stripe's own event ID — Stripe redelivers on timeout/5xx.
	first, err := db.MarkEventSeen(ctx, h.Pool, "stripe", event.ID)
	if err != nil {
		log.Error("dedupe check failed", "step", "dedupe", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !first {
		log.Info("duplicate event skipped")
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := h.handleEvent(ctx, log, event); err != nil {
		// Release the claim and 500 so Stripe's retry processes it again.
		log.Error("event handling failed; Stripe will retry", "err", err)
		if err := db.ReleaseEvent(ctx, h.Pool, "stripe", event.ID); err != nil {
			log.Error("release dedupe claim failed", "err", err)
		}
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// handleEvent returns an error only for failures worth retrying. Problems a
// retry can't fix are logged here and swallowed.
func (h *Handler) handleEvent(ctx context.Context, log *slog.Logger, event stripe.Event) error {
	switch event.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded",
		"checkout.session.async_payment_failed", "checkout.session.expired":
	default:
		return nil // Ignore other event types.
	}

	var sess stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
		log.Error("bad checkout session payload", "err", err)
		return nil
	}
	log = log.With("checkout_session_id", sess.ID, "invoice_no", sess.ClientReferenceID)
	if sess.ClientReferenceID == "" {
		log.Error("checkout session has no client_reference_id; not one of this bot's payment attempts")
		return nil
	}

	r := payments.StripeResult(&sess)
	if event.Type == "checkout.session.async_payment_failed" {
		r.Status, r.RespDesc = payments.ResultFailed, "async payment failed"
	}
	if r.Status == payments.ResultPending {
		log.Info("checkout completed; payment still processing", "payment_status", sess.PaymentStatus)
		return nil
	}

	applied, err := h.Payments.Apply(ctx, r)
	if err != nil {
		return err
	}
	log = log.With("order_id", applied.OrderID)

	// The result is recorded from here on, so message failures are recorded
	// on the order rather than retried (a retry could double-send).
	switch {
	case applied.Outcome == payments.ApplyPaid:
		h.Notify.Paid(ctx, log, applied)
	case applied.Outcome == payments.ApplyFailed && event.Type == "checkout.session.async_payment_failed":
		// An expired session needs no message: the customer just didn't pay.
		// Only PayNow is asynchronous; cards fail on the Checkout page.
		h.Notify.Failed(ctx, log, applied, "Stripe Payment")
	}
	return nil
}
