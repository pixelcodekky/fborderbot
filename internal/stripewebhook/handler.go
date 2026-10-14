package stripewebhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v78"
	"github.com/stripe/stripe-go/v78/webhook"

	"fb-order-bot/internal/db"
	"fb-order-bot/internal/messenger"
	"fb-order-bot/internal/obs"
)

type Handler struct {
	Pool          *pgxpool.Pool
	WebhookSecret string
	// SendEmail delivers an order confirmation; wire up SES/SendGrid here.
	SendEmail func(to, subject, body string) error
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
	case "checkout.session.completed", "checkout.session.expired":
	default:
		return nil // Ignore other event types.
	}

	var sess stripe.CheckoutSession
	if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
		log.Error("bad checkout session payload", "err", err)
		return nil
	}
	log = log.With("checkout_session_id", sess.ID)

	if event.Type == "checkout.session.expired" {
		return h.markOrderStatus(ctx, log, sess.ID, "payment_failed")
	}
	return h.handleCheckoutCompleted(ctx, log, &sess)
}

func (h *Handler) handleCheckoutCompleted(ctx context.Context, log *slog.Logger, sess *stripe.CheckoutSession) error {
	orderIDStr, ok := sess.Metadata["order_id"]
	if !ok {
		log.Error("session missing order_id metadata")
		return nil
	}
	orderID, err := strconv.ParseInt(orderIDStr, 10, 64)
	if err != nil {
		log.Error("invalid order_id metadata", "order_id_raw", orderIDStr, "err", err)
		return nil
	}
	log = log.With("order_id", orderID)

	var (
		psid            *string
		email           *string
		pageAccessToken string
		productName     string
	)
	err = h.Pool.QueryRow(ctx, `
		SELECT o.psid, o.customer_email, p.page_access_token, COALESCE(pr.name, 'your order')
		FROM orders o
		JOIN pages p ON p.id = o.page_id
		LEFT JOIN products pr ON pr.id = o.product_id
		WHERE o.id = $1`,
		orderID,
	).Scan(&psid, &email, &pageAccessToken, &productName)
	if errors.Is(err, pgx.ErrNoRows) {
		log.Error("paid session references unknown order", "step", "lookup_order")
		return nil
	}
	if err != nil {
		return fmt.Errorf("look up order: %w", err)
	}

	paymentMethod := "card"
	if len(sess.PaymentMethodTypes) > 0 {
		paymentMethod = sess.PaymentMethodTypes[0]
	}

	var paymentIntentID string
	if sess.PaymentIntent != nil {
		paymentIntentID = sess.PaymentIntent.ID
	}

	_, err = h.Pool.Exec(ctx, `
		UPDATE orders
		SET status = 'paid',
		    stripe_payment_intent_id = $1,
		    payment_method = $2,
		    last_error = NULL, last_error_step = NULL, last_error_at = NULL,
		    updated_at = now()
		WHERE id = $3`,
		paymentIntentID, paymentMethod, orderID,
	)
	if err != nil {
		return fmt.Errorf("mark order paid: %w", err)
	}
	log.Info("order paid", "payment_method", paymentMethod, "payment_intent_id", paymentIntentID)

	// The payment is recorded from here on, so confirmation failures are
	// recorded on the order rather than retried (a retry could double-send).
	if psid != nil {
		client := messenger.New(pageAccessToken)
		msg := fmt.Sprintf("Payment received ✅ Your order for %s is confirmed. We'll be in touch about delivery!", productName)
		if err := client.SendText(*psid, msg); err != nil {
			h.recordOrderError(ctx, log, orderID, "confirmation_message", err)
		}
	} else {
		log.Warn("order has no psid; skipping Messenger confirmation")
	}

	if email != nil && *email != "" && h.SendEmail != nil {
		body := fmt.Sprintf("Hi, thanks for your order! We've confirmed payment for: %s.", productName)
		if err := h.SendEmail(*email, "Order Confirmation", body); err != nil {
			h.recordOrderError(ctx, log, orderID, "confirmation_email", err)
		}
	}
	return nil
}

func (h *Handler) markOrderStatus(ctx context.Context, log *slog.Logger, checkoutSessionID, status string) error {
	tag, err := h.Pool.Exec(ctx,
		`UPDATE orders SET status = $1, updated_at = now() WHERE stripe_checkout_session_id = $2`,
		status, checkoutSessionID,
	)
	if err != nil {
		return fmt.Errorf("mark order %s: %w", status, err)
	}
	if tag.RowsAffected() == 0 {
		log.Warn("no order matches checkout session", "status", status)
		return nil
	}
	log.Info("order status changed", "status", status)
	return nil
}

func (h *Handler) recordOrderError(ctx context.Context, log *slog.Logger, orderID int64, step string, err error) {
	log.Error(step+" failed", "step", step, "err", err)
	if dbErr := db.RecordOrderError(ctx, h.Pool, orderID, step, err); dbErr != nil {
		log.Error("record order error failed", "err", dbErr)
	}
}
