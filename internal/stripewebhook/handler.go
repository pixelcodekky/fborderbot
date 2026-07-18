package stripewebhook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v78"
	"github.com/stripe/stripe-go/v78/webhook"

	"fb-order-bot/internal/db"
	"fb-order-bot/internal/messenger"
)

type Handler struct {
	Pool          *pgxpool.Pool
	WebhookSecret string
	// SendEmail delivers an order confirmation; wire up SES/SendGrid here.
	SendEmail func(to, subject, body string) error
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const maxBodyBytes = int64(65536)
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	event, err := webhook.ConstructEvent(payload, r.Header.Get("Stripe-Signature"), h.WebhookSecret)
	if err != nil {
		log.Printf("stripewebhook: signature verification failed: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	ctx := context.Background()

	// Dedupe on Stripe's own event ID — Stripe redelivers on timeout/5xx.
	first, err := db.MarkEventSeen(ctx, h.Pool, "stripe", event.ID)
	if err != nil {
		log.Printf("stripewebhook: dedupe check failed: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !first {
		w.WriteHeader(http.StatusOK)
		return
	}

	switch event.Type {
	case "checkout.session.completed":
		var sess stripe.CheckoutSession
		if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
			log.Printf("stripewebhook: bad session payload: %v", err)
			break
		}
		h.handleCheckoutCompleted(ctx, &sess)
	case "checkout.session.expired":
		var sess stripe.CheckoutSession
		if err := json.Unmarshal(event.Data.Raw, &sess); err != nil {
			break
		}
		h.markOrderStatus(ctx, sess.ID, "payment_failed")
	default:
		// Ignore other event types.
	}

	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleCheckoutCompleted(ctx context.Context, sess *stripe.CheckoutSession) {
	orderIDStr, ok := sess.Metadata["order_id"]
	if !ok {
		log.Printf("stripewebhook: session %s missing order_id metadata", sess.ID)
		return
	}
	orderID, err := strconv.ParseInt(orderIDStr, 10, 64)
	if err != nil {
		return
	}

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
	if err != nil {
		log.Printf("stripewebhook: order %d lookup failed: %v", orderID, err)
		return
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
		    updated_at = now()
		WHERE id = $3`,
		paymentIntentID, paymentMethod, orderID,
	)
	if err != nil {
		log.Printf("stripewebhook: failed to update order %d: %v", orderID, err)
		return
	}

	if psid != nil {
		client := messenger.New(pageAccessToken)
		msg := fmt.Sprintf("Payment received ✅ Your order for %s is confirmed. We'll be in touch about delivery!", productName)
		if err := client.SendText(*psid, msg); err != nil {
			log.Printf("stripewebhook: messenger confirmation failed for order %d: %v", orderID, err)
		}
	}

	if email != nil && *email != "" && h.SendEmail != nil {
		body := fmt.Sprintf("Hi, thanks for your order! We've confirmed payment for: %s.", productName)
		if err := h.SendEmail(*email, "Order Confirmation", body); err != nil {
			log.Printf("stripewebhook: email confirmation failed for order %d: %v", orderID, err)
		}
	}
}

func (h *Handler) markOrderStatus(ctx context.Context, checkoutSessionID, status string) {
	_, err := h.Pool.Exec(ctx,
		`UPDATE orders SET status = $1, updated_at = now() WHERE stripe_checkout_session_id = $2`,
		status, checkoutSessionID,
	)
	if err != nil {
		log.Printf("stripewebhook: failed to mark order status: %v", err)
	}
}
