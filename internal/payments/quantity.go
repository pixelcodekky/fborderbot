package payments

import (
	"context"
	"errors"
	"fmt"
)

// MaxQuantity is the most a customer can order in one go.
const MaxQuantity = 10

// ErrPaymentOpen means the order has a live payment page, or one whose result
// isn't known yet; its amount can't change until that is settled.
var ErrPaymentOpen = errors.New("order has an open payment")

// ErrNotAwaitingPayment means the order is past choosing (paid, cancelled...).
var ErrNotAwaitingPayment = errors.New("order is no longer awaiting payment")

// ErrNoPrice means the order has no unit price to multiply.
var ErrNoPrice = errors.New("order has no unit price")

// SetQuantity sets the order's quantity, and its amount to unit price ×
// quantity. The amount is what a payment page charges, so it can't change
// while one is open: then it returns ErrPaymentOpen with that page (see
// Current), or ErrAlreadyPaid / ErrPaymentInProgress. An expired page is
// settled first, like Pay does.
func (s *Service) SetQuantity(ctx context.Context, orderID int64, qty int) (amountCents int64, res PayResult, err error) {
	if qty < 1 || qty > MaxQuantity {
		return 0, res, fmt.Errorf("quantity %d out of range 1-%d", qty, MaxQuantity)
	}
	res, open, err := s.Current(ctx, orderID)
	if err != nil {
		return 0, res, err
	}
	if open {
		return 0, res, ErrPaymentOpen
	}

	// Locked like create, so a payment page can't be claimed for the old
	// amount while this changes it.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, res, err
	}
	defer tx.Rollback(ctx)
	var status string
	var unit *int64
	if err := tx.QueryRow(ctx,
		`SELECT status, unit_price_cents FROM orders WHERE id = $1 FOR UPDATE`, orderID,
	).Scan(&status, &unit); err != nil {
		return 0, res, fmt.Errorf("lock order: %w", err)
	}
	if status != "pending_form" && status != "pending_payment" {
		return 0, res, ErrNotAwaitingPayment
	}
	if unit == nil {
		return 0, res, ErrNoPrice
	}
	// Read after the lock, so it sees a claim committed just before.
	var claimed bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM payment_attempts WHERE order_id = $1 AND status IN ('pending', 'succeeded'))`,
		orderID,
	).Scan(&claimed); err != nil {
		return 0, res, err
	}
	if claimed {
		return 0, PayResult{Outcome: PayChecking}, ErrPaymentOpen
	}
	amountCents = *unit * int64(qty)
	if _, err := tx.Exec(ctx,
		`UPDATE orders SET quantity = $1, amount_cents = $2, updated_at = now() WHERE id = $3`,
		qty, amountCents, orderID,
	); err != nil {
		return 0, res, fmt.Errorf("set quantity: %w", err)
	}
	return amountCents, res, tx.Commit(ctx)
}
