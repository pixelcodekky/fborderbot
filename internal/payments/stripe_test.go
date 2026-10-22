package payments

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v78"
)

// fakeSessions is a stand-in for Stripe's Checkout Sessions API.
type fakeSessions struct {
	mu       sync.Mutex
	params   []*stripe.CheckoutSessionParams
	sessions map[string]*stripe.CheckoutSession
}

func (f *fakeSessions) New(p *stripe.CheckoutSessionParams) (*stripe.CheckoutSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.params = append(f.params, p)
	id := fmt.Sprintf("cs_%d", len(f.params))
	price := p.LineItems[0].PriceData
	f.sessions[id] = &stripe.CheckoutSession{
		ID: id, URL: "https://checkout.example/" + id,
		Status: stripe.CheckoutSessionStatusOpen, PaymentStatus: stripe.CheckoutSessionPaymentStatusUnpaid,
		ClientReferenceID: *p.ClientReferenceID, AmountTotal: *price.UnitAmount, Currency: stripe.Currency(*price.Currency),
	}
	return f.get(id)
}

func (f *fakeSessions) Get(id string, _ *stripe.CheckoutSessionParams) (*stripe.CheckoutSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.get(id)
}

func (f *fakeSessions) Expire(id string, _ *stripe.CheckoutSessionExpireParams) (*stripe.CheckoutSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[id]
	if s == nil || s.Status != stripe.CheckoutSessionStatusOpen {
		return nil, errors.New("session is not open")
	}
	s.Status = stripe.CheckoutSessionStatusExpired
	return f.get(id)
}

func (f *fakeSessions) get(id string) (*stripe.CheckoutSession, error) {
	s := f.sessions[id]
	if s == nil {
		return nil, errors.New("no such session")
	}
	c := *s
	return &c, nil
}

// complete makes the session paid, as if the customer paid on it.
func (f *fakeSessions) complete(id string) *stripe.CheckoutSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sessions[id]
	s.Status, s.PaymentStatus = stripe.CheckoutSessionStatusComplete, stripe.CheckoutSessionPaymentStatusPaid
	s.PaymentIntent = &stripe.PaymentIntent{ID: "pi_" + id}
	s.PaymentMethodTypes = []string{"paynow"}
	c := *s
	return &c
}

func (f *fakeSessions) status(id string) stripe.CheckoutSessionStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[id].Status
}

// withStripe adds PayNow to the test env.
func withStripe(e *testEnv) *fakeSessions {
	f := &fakeSessions{sessions: map[string]*stripe.CheckoutSession{}}
	e.s.Providers[MethodPayNow] = &Stripe{Sessions: f, Methods: []string{"paynow"}, SuccessURL: "https://shop/ok", CancelURL: "https://shop/cancel"}
	return f
}

func (e *testEnv) payWith(t *testing.T, method string) (PayResult, error) {
	t.Helper()
	return e.s.Pay(context.Background(), method, PayRequest{OrderID: e.orderID, AmountCents: 10000, Currency: "sgd", Description: "Order"})
}

func sessionID(url string) string { return url[len("https://checkout.example/"):] }

func TestPayNowLinkBlocksOtherMethodUntilClosed(t *testing.T) {
	e := newTestEnv(t)
	f := withStripe(e)
	first, err := e.payWith(t, MethodPayNow)
	if err != nil || first.Outcome != PayNewLink || first.Method != MethodPayNow {
		t.Fatalf("paynow: %+v %v", first, err)
	}
	p := f.params[0]
	if *p.ClientReferenceID == "" || *p.ExpiresAt != e.now.Add(31*time.Minute).Unix() || p.IdempotencyKey == nil {
		t.Errorf("session params: ref %q, expires %d, idempotency %v", *p.ClientReferenceID, *p.ExpiresAt, p.IdempotencyKey)
	}
	if !first.ExpiresAt.Equal(e.now.Add(30 * time.Minute)) {
		t.Errorf("expires at %v", first.ExpiresAt)
	}

	// PayNow again: the same session. 2C2P: told to use the PayNow link.
	if again, _ := e.payWith(t, MethodPayNow); again.Outcome != PaySameLink || again.URL != first.URL {
		t.Errorf("paynow again: %+v", again)
	}
	other, err := e.payWith(t, Method2C2P)
	if err != nil || other.Outcome != PaySameLink || other.Method != MethodPayNow {
		t.Errorf("2c2p while paynow is live: %+v %v", other, err)
	}
	if len(f.params) != 1 || e.fake.tokens.Load() != 0 {
		t.Errorf("%d sessions, %d 2c2p tokens; want 1 and 0", len(f.params), e.fake.tokens.Load())
	}

	// After our expiry the session, still open at Stripe, is expired there
	// before the 2C2P page is made.
	e.advance(30 * time.Minute)
	next, err := e.payWith(t, Method2C2P)
	if err != nil || next.Outcome != PayNewLink || next.Method != Method2C2P {
		t.Fatalf("2c2p after expiry: %+v %v", next, err)
	}
	if s := f.status(sessionID(first.URL)); s != stripe.CheckoutSessionStatusExpired {
		t.Errorf("stripe session %q, want expired", s)
	}
}

func Test2C2PLinkBlocksPayNow(t *testing.T) {
	e := newTestEnv(t)
	f := withStripe(e)
	e.payWith(t, Method2C2P)
	res, err := e.payWith(t, MethodPayNow)
	if err != nil || res.Outcome != PaySameLink || res.Method != Method2C2P {
		t.Fatalf("paynow while 2c2p is live: %+v %v", res, err)
	}
	if len(f.params) != 0 {
		t.Errorf("%d stripe sessions created, want 0", len(f.params))
	}
}

func TestExpiredPayNowFoundPaidIsNotReplaced(t *testing.T) {
	e := newTestEnv(t)
	f := withStripe(e)
	first, _ := e.payWith(t, MethodPayNow)
	f.complete(sessionID(first.URL)) // paid, but the webhook was lost
	e.advance(31 * time.Minute)

	res, err := e.payWith(t, Method2C2P)
	if err != nil || res.Outcome != PayAlreadyPaid {
		t.Fatalf("got %+v %v", res, err)
	}
	var status, method, tranRef string
	e.s.Pool.QueryRow(context.Background(),
		`SELECT o.status, o.payment_method, a.tran_ref FROM orders o JOIN payment_attempts a ON a.order_id = o.id WHERE o.id = $1`,
		e.orderID).Scan(&status, &method, &tranRef)
	if status != "paid" || method != MethodPayNow || tranRef != "pi_"+sessionID(first.URL) {
		t.Errorf("order %q via %q, tran_ref %q", status, method, tranRef)
	}
	if e.fake.tokens.Load() != 0 {
		t.Errorf("2c2p page made for a paid order")
	}
}

func TestStripeWebhookResults(t *testing.T) {
	e := newTestEnv(t)
	f := withStripe(e)
	ctx := context.Background()
	first, _ := e.payWith(t, MethodPayNow)
	id := sessionID(first.URL)

	// Completed but still processing (async method): nothing applied.
	sess, _ := f.Get(id, nil)
	sess.Status = stripe.CheckoutSessionStatusComplete
	if r := StripeResult(sess); r.Status != ResultPending {
		t.Errorf("processing session: %v", r.Status)
	}

	paid := StripeResult(f.complete(id))
	if ap, err := e.s.Apply(ctx, paid); err != nil || ap.Outcome != ApplyPaid {
		t.Fatalf("paid: %v %v", ap.Outcome, err)
	}
	if ap, _ := e.s.Apply(ctx, paid); ap.Outcome != ApplyAlreadyFinal {
		t.Errorf("redelivered: %v", ap.Outcome)
	}
	if s := e.orderStatus(t); s != "paid" {
		t.Errorf("order status %q", s)
	}
}

func TestStripeExpiredSessionAllowsNewLink(t *testing.T) {
	e := newTestEnv(t)
	f := withStripe(e)
	first, _ := e.payWith(t, MethodPayNow)
	id := sessionID(first.URL)
	exp, _ := f.Expire(id, nil)
	if ap, _ := e.s.Apply(context.Background(), StripeResult(exp)); ap.Outcome != ApplyFailed {
		t.Fatalf("expired session: %v", ap.Outcome)
	}
	if s := e.orderStatus(t); s != "pending_payment" {
		t.Errorf("order status %q, want pending_payment", s)
	}
	next, err := e.payWith(t, MethodPayNow)
	if err != nil || next.Outcome != PayNewLink || next.URL == first.URL {
		t.Fatalf("new link: %+v %v", next, err)
	}
}

func TestPayUnavailableMethod(t *testing.T) {
	s := &Service{Providers: map[string]Provider{}}
	if _, err := s.Pay(context.Background(), MethodPayNow, PayRequest{OrderID: 1}); !errors.Is(err, ErrMethodUnavailable) {
		t.Errorf("got %v", err)
	}
}
