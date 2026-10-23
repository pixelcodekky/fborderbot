package payments

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func (e *testEnv) payAmount(t *testing.T, cents int64) (PayResult, error) {
	t.Helper()
	return e.s.Pay(context.Background(), Method2C2P, PayRequest{OrderID: e.orderID, AmountCents: cents, Currency: "sgd", Description: "Order"})
}

func TestSetQuantityLockedWhileLinkIsLive(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if amount, _, err := e.s.SetQuantity(ctx, e.orderID, 3); err != nil || amount != 30000 {
		t.Fatalf("set 3: %d %v", amount, err)
	}
	link, err := e.payAmount(t, 30000)
	if err != nil || link.Outcome != PayNewLink {
		t.Fatalf("pay: %+v %v", link, err)
	}
	var charged int64
	e.s.Pool.QueryRow(ctx, `SELECT amount_cents FROM payment_attempts WHERE order_id = $1`, e.orderID).Scan(&charged)
	if charged != 30000 {
		t.Errorf("attempt charges %d, want 30000", charged)
	}

	_, res, err := e.s.SetQuantity(ctx, e.orderID, 2)
	if !errors.Is(err, ErrPaymentOpen) || res.Outcome != PaySameLink || res.URL != link.URL {
		t.Fatalf("change while live: %+v %v", res, err)
	}

	// Expired and unpaid: settled, then the change goes through.
	e.advance(21 * time.Minute)
	e.fake.inquiry = map[string]any{"respCode": "4200"}
	if amount, _, err := e.s.SetQuantity(ctx, e.orderID, 2); err != nil || amount != 20000 {
		t.Fatalf("change after expiry: %d %v", amount, err)
	}
}

func TestSetQuantityRejects(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	for _, q := range []int{0, MaxQuantity + 1} {
		if _, _, err := e.s.SetQuantity(ctx, e.orderID, q); err == nil {
			t.Errorf("quantity %d accepted", q)
		}
	}
	link, _ := e.pay(t)
	e.s.Apply(ctx, paidResult(invoiceOf(link.URL)))
	if _, _, err := e.s.SetQuantity(ctx, e.orderID, 2); !errors.Is(err, ErrAlreadyPaid) {
		t.Errorf("after paid: %v", err)
	}
}

func TestPayWithStaleAmountIsRefused(t *testing.T) {
	e := newTestEnv(t)
	e.s.SetQuantity(context.Background(), e.orderID, 2)
	if _, err := e.payAmount(t, 10000); !errors.Is(err, ErrOrderChanged) {
		t.Fatalf("got %v", err)
	}
	if n := e.fake.tokens.Load(); n != 0 {
		t.Errorf("%d payment pages made for a stale amount", n)
	}
	if res, err := e.payAmount(t, 20000); err != nil || res.Outcome != PayNewLink {
		t.Errorf("with the new amount: %+v %v", res, err)
	}
}

// A quantity change racing a payment tap never leaves a page charging an
// amount the order no longer has.
func TestQuantityRacingPay(t *testing.T) {
	ctx := context.Background()
	for round := range 5 {
		e := newTestEnv(t)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); e.s.SetQuantity(ctx, e.orderID, 3) }()
		go func() { defer wg.Done(); e.payAmount(t, 10000) }()
		wg.Wait()

		var order int64
		var attempt *int64
		e.s.Pool.QueryRow(ctx,
			`SELECT o.amount_cents, (SELECT a.amount_cents FROM payment_attempts a WHERE a.order_id = o.id AND a.status = 'pending')
			 FROM orders o WHERE o.id = $1`, e.orderID).Scan(&order, &attempt)
		if attempt != nil && *attempt != order {
			t.Fatalf("round %d: page charges %d, order is %d", round, *attempt, order)
		}
	}
}
