// Package notify tells the customer about a recorded payment result, whichever
// way it arrived: a provider's webhook or the customer's browser coming back
// to the payment return page.
package notify

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"fb-order-bot/internal/db"
	"fb-order-bot/internal/messenger"
	"fb-order-bot/internal/payments"
)

type Customer struct {
	Pool         *pgxpool.Pool
	NewMessenger func(pageAccessToken string) *messenger.Client
	// PublicBaseURL is where this bot is reachable, for receipt links (see
	// payments.ReceiptURL). Empty sends no link.
	PublicBaseURL string
	// SendEmail delivers an order confirmation; nil sends none.
	SendEmail func(to, subject, body string) error
}

// Paid confirms a payment that Apply reported as ApplyPaid. Apply reports
// that once per payment, so the customer isn't told twice. Failures are
// recorded on the order rather than returned: the payment is already
// recorded, and a retry could double-send.
func (c *Customer) Paid(ctx context.Context, log *slog.Logger, ap payments.Applied) {
	msg := fmt.Sprintf("Payment received ✅ Order #%d for %s is confirmed. We'll be in touch about delivery!", ap.OrderID, ap.ProductName)
	if receipt := payments.ReceiptURL(c.PublicBaseURL, ap.ReceiptToken); receipt != "" && ap.PSID != nil {
		err := c.NewMessenger(ap.PageAccessToken).SendButtons(*ap.PSID, msg,
			[]messenger.Button{{Type: "web_url", Title: "View receipt", URL: receipt}})
		if err != nil {
			c.recordOrderError(ctx, log, ap.OrderID, "confirmation_message", err)
		}
	} else {
		c.Text(ctx, log, ap, "confirmation_message", msg)
	}

	if c.SendEmail == nil {
		return
	}
	var email *string
	if err := c.Pool.QueryRow(ctx, `SELECT customer_email FROM orders WHERE id = $1`, ap.OrderID).Scan(&email); err != nil {
		c.recordOrderError(ctx, log, ap.OrderID, "confirmation_email", fmt.Errorf("look up email: %w", err))
		return
	}
	if email != nil && *email != "" {
		body := fmt.Sprintf("Hi, thanks for your order! We've confirmed payment for: %s.", ap.ProductName)
		if err := c.SendEmail(*email, "Order Confirmation", body); err != nil {
			c.recordOrderError(ctx, log, ap.OrderID, "confirmation_email", err)
		}
	}
}

// Failed tells the customer their payment didn't go through and to tap
// button (e.g. "Stripe Payment") to retry.
func (c *Customer) Failed(ctx context.Context, log *slog.Logger, ap payments.Applied, button string) {
	c.Text(ctx, log, ap, "payment_failed_message",
		fmt.Sprintf("Your payment for order #%d didn't go through. Please tap \"%s\" again to retry.", ap.OrderID, button))
}

// Text sends msg to the order's customer; step names it in logs and errors.
func (c *Customer) Text(ctx context.Context, log *slog.Logger, ap payments.Applied, step, msg string) {
	if ap.PSID == nil {
		log.Warn("order has no psid; skipping Messenger message", "step", step)
		return
	}
	if err := c.NewMessenger(ap.PageAccessToken).SendText(*ap.PSID, msg); err != nil {
		c.recordOrderError(ctx, log, ap.OrderID, step, err)
	}
}

func (c *Customer) recordOrderError(ctx context.Context, log *slog.Logger, orderID int64, step string, err error) {
	log.Error(step+" failed", "step", step, "err", err)
	if dbErr := db.RecordOrderError(ctx, c.Pool, orderID, step, err); dbErr != nil {
		log.Error("record order error failed", "err", dbErr)
	}
}
