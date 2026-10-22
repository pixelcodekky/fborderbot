package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"fb-order-bot/internal/twoc2p"
)

func TestInvoiceNo(t *testing.T) {
	now := time.UnixMilli(1792459910123)
	a, b := InvoiceNo(42, now), InvoiceNo(42, now)
	if !strings.HasPrefix(a, "FB42T1792459910123R") || len(a) > 50 || a == b {
		t.Fatalf("InvoiceNo = %q, %q", a, b)
	}
}

func TestParseCents(t *testing.T) {
	cases := []struct {
		amount string
		cents  int64
		ok     bool
	}{
		{"100.00", 10000, true},
		{"100", 10000, true},
		{"100.5", 10050, true},
		{"100.500", 10050, true},
		{"100.501", 0, false},
		{"99.99", 9999, true},
		{"", 0, false},
		{".5", 0, false},
		{"-1.00", 0, false},
		{"1e3", 0, false},
	}
	for _, c := range cases {
		if cents, ok := ParseCents(c.amount); ok != c.ok || (ok && cents != c.cents) {
			t.Errorf("ParseCents(%q) = %d, %v", c.amount, cents, ok)
		}
	}
}

// --- integration tests: need TEST_DATABASE_URL (a scratch database) ---

const testSecret = "secret"

// fake2C2P is a 2C2P sandbox stand-in. inquiry decides Payment Inquiry's
// answer; nil makes it fail at the HTTP level.
type fake2C2P struct {
	tokens  atomic.Int32
	mu      sync.Mutex
	inquiry map[string]any
}

func (f *fake2C2P) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct{ Payload string }
	json.Unmarshal(body, &req)
	claims := decodeTestJWT(req.Payload)
	switch r.URL.Path {
	case "/payment/4.3/paymentToken":
		f.tokens.Add(1)
		time.Sleep(20 * time.Millisecond) // widen the race window
		writeSigned(w, map[string]any{"respCode": "0000", "webPaymentUrl": "https://pay.example/" + claims["invoiceNo"].(string)})
	case "/payment/4.3/paymentInquiry":
		f.mu.Lock()
		ans := f.inquiry
		f.mu.Unlock()
		if ans == nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		out := map[string]any{"merchantID": "M1", "invoiceNo": claims["invoiceNo"]}
		maps.Copy(out, ans)
		writeSigned(w, out)
	}
}

func writeSigned(w http.ResponseWriter, claims map[string]any) {
	b, _ := json.Marshal(claims)
	in := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(in))
	json.NewEncoder(w).Encode(map[string]string{"payload": in + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))})
}

func decodeTestJWT(token string) map[string]any {
	parts := strings.Split(token, ".")
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

type testEnv struct {
	s       *Service
	fake    *fake2C2P
	orderID int64
	now     time.Time
}

func (e *testEnv) advance(d time.Duration) { e.now = e.now.Add(d) }

// newTestEnv gives each test its own schema with one page and one order of
// SGD 100.
func newTestEnv(t *testing.T) *testEnv {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("t%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	pool, err := pgxpool.New(ctx, dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ddl, err := os.ReadFile("../../schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(ddl)); err != nil {
		t.Fatal(err)
	}
	var orderID int64
	err = pool.QueryRow(ctx,
		`WITH p AS (INSERT INTO pages (fb_page_id, name, page_access_token) VALUES ('1', 'Shop', 'tok') RETURNING id)
		 INSERT INTO orders (page_id, fb_comment_id, psid, amount_cents, currency)
		 SELECT id, 'c1', 'psid-1', 10000, 'sgd' FROM p RETURNING id`,
	).Scan(&orderID)
	if err != nil {
		t.Fatal(err)
	}

	fake := &fake2C2P{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	env := &testEnv{fake: fake, orderID: orderID, now: time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)}
	env.s = &Service{
		Pool: pool,
		Providers: map[string]Provider{
			Method2C2P: &TwoC2P{
				Client:     twoc2p.New(srv.URL, "M1", testSecret, "https://shop/return", "https://shop/webhooks/2c2p"),
				PageExpiry: 20 * time.Minute,
			},
		},
		Now: func() time.Time { return env.now },
	}
	return env
}

func (e *testEnv) pay(t *testing.T) (PayResult, error) {
	t.Helper()
	return e.s.Pay(context.Background(), Method2C2P, PayRequest{OrderID: e.orderID, AmountCents: 10000, Currency: "sgd", Description: "Order"})
}

func (e *testEnv) orderStatus(t *testing.T) string {
	t.Helper()
	var s string
	if err := e.s.Pool.QueryRow(context.Background(), `SELECT status FROM orders WHERE id = $1`, e.orderID).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func paidResult(invoiceNo string) Result {
	return TwoC2PResult(&twoc2p.Result{MerchantID: "M1", InvoiceNo: invoiceNo, Amount: "100.00", CurrencyCode: "SGD", TranRef: "T-" + invoiceNo, RespCode: "0000"})
}

func invoiceOf(url string) string { return strings.TrimPrefix(url, "https://pay.example/") }

func TestPaySendsSameLinkWhileLive(t *testing.T) {
	e := newTestEnv(t)
	first, err := e.pay(t)
	if err != nil || first.Outcome != PayNewLink {
		t.Fatalf("first: %+v %v", first, err)
	}
	e.advance(10 * time.Minute)
	again, err := e.pay(t)
	if err != nil || again.Outcome != PaySameLink || again.URL != first.URL {
		t.Fatalf("second: %+v %v", again, err)
	}
	if n := e.fake.tokens.Load(); n != 1 {
		t.Errorf("%d payment tokens created, want 1", n)
	}
	if s := e.orderStatus(t); s != "pending_payment" {
		t.Errorf("order status %q", s)
	}
}

func TestConcurrentTapsMakeOnePage(t *testing.T) {
	e := newTestEnv(t)
	var wg sync.WaitGroup
	var newLinks, inProgress atomic.Int32
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := e.pay(t)
			switch {
			case errors.Is(err, ErrPaymentInProgress):
				inProgress.Add(1)
			case err != nil:
				t.Error(err)
			case res.Outcome == PayNewLink:
				newLinks.Add(1)
			}
		}()
	}
	wg.Wait()
	if newLinks.Load() != 1 || e.fake.tokens.Load() != 1 {
		t.Errorf("new links %d, tokens %d; want 1 and 1 (in progress: %d)", newLinks.Load(), e.fake.tokens.Load(), inProgress.Load())
	}
}

func TestExpiredUnpaidPageIsReplaced(t *testing.T) {
	e := newTestEnv(t)
	first, _ := e.pay(t)
	e.advance(21 * time.Minute)
	e.fake.inquiry = map[string]any{"respCode": "4200", "respDesc": "Payment expired"}

	next, err := e.pay(t)
	if err != nil || next.Outcome != PayNewLink || next.URL == first.URL {
		t.Fatalf("after expiry: %+v %v", next, err)
	}
	var retryOf *int64
	var firstStatus string
	ctx := context.Background()
	e.s.Pool.QueryRow(ctx, `SELECT status FROM payment_attempts WHERE invoice_no = $1`, invoiceOf(first.URL)).Scan(&firstStatus)
	e.s.Pool.QueryRow(ctx, `SELECT retry_of FROM payment_attempts WHERE invoice_no = $1`, invoiceOf(next.URL)).Scan(&retryOf)
	if firstStatus != "failed" || retryOf == nil {
		t.Errorf("first attempt %q, retry_of %v", firstStatus, retryOf)
	}
}

func TestExpiredPageFoundPaidIsNotReplaced(t *testing.T) {
	e := newTestEnv(t)
	e.pay(t)
	e.advance(21 * time.Minute)
	// The notification was lost, but the customer paid.
	e.fake.inquiry = map[string]any{"respCode": "0000", "amount": 100.00, "currencyCode": "SGD", "tranRef": "T1"}

	res, err := e.pay(t)
	if err != nil || res.Outcome != PayAlreadyPaid {
		t.Fatalf("got %+v %v", res, err)
	}
	if n := e.fake.tokens.Load(); n != 1 {
		t.Errorf("%d payment tokens created, want 1", n)
	}
	if s := e.orderStatus(t); s != "paid" {
		t.Errorf("order status %q", s)
	}
	if _, err := e.pay(t); !errors.Is(err, ErrAlreadyPaid) {
		t.Errorf("pay after paid: %v", err)
	}
}

func TestExpiredPageUnknownWaits(t *testing.T) {
	e := newTestEnv(t)
	e.pay(t)
	e.advance(21 * time.Minute)
	for _, ans := range []map[string]any{nil, {"respCode": "2001", "respDesc": "In progress"}} {
		e.fake.inquiry = ans
		res, err := e.pay(t)
		if err != nil || res.Outcome != PayChecking {
			t.Errorf("inquiry %v: got %+v %v", ans, res, err)
		}
	}
	if n := e.fake.tokens.Load(); n != 1 {
		t.Errorf("%d payment tokens created, want 1", n)
	}
}

func TestAbandonedAttemptIsReplaced(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	// A crash between claiming the attempt and storing its URL.
	_, err := e.s.Pool.Exec(ctx,
		`INSERT INTO payment_attempts (order_id, invoice_no, amount_cents, currency, expires_at, created_at)
		 VALUES ($1, 'FB1T1', 10000, 'sgd', $2, $3)`,
		e.orderID, e.now.Add(20*time.Minute), e.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pay(t); !errors.Is(err, ErrPaymentInProgress) {
		t.Fatalf("right after the claim: %v", err)
	}
	e.advance(2 * time.Minute)
	if res, err := e.pay(t); err != nil || res.Outcome != PayNewLink {
		t.Fatalf("after creatingTimeout: %+v %v", res, err)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	res, _ := e.pay(t)
	r := paidResult(invoiceOf(res.URL))

	var wg sync.WaitGroup
	outcomes := make([]ApplyOutcome, 5)
	for i := range outcomes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ap, err := e.s.Apply(ctx, r)
			if err != nil {
				t.Error(err)
			}
			outcomes[i] = ap.Outcome
		}()
	}
	wg.Wait()
	var paid int
	for _, o := range outcomes {
		if o == ApplyPaid {
			paid++
		} else if o != ApplyAlreadyFinal {
			t.Errorf("outcome %v", o)
		}
	}
	if paid != 1 {
		t.Errorf("%d ApplyPaid, want 1: %v", paid, outcomes)
	}
	if s := e.orderStatus(t); s != "paid" {
		t.Errorf("order status %q", s)
	}
}

func TestLatePaymentOnReplacedPageIsDoublePayment(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	first, _ := e.pay(t)
	e.advance(21 * time.Minute)
	e.fake.inquiry = map[string]any{"respCode": "4200"}
	second, _ := e.pay(t)

	if ap, _ := e.s.Apply(ctx, paidResult(invoiceOf(second.URL))); ap.Outcome != ApplyPaid {
		t.Fatalf("second page: %v", ap.Outcome)
	}
	// 2C2P shouldn't take money after paymentExpiry, but if it does, it's
	// caught and flagged for a refund instead of passing silently.
	ap, err := e.s.Apply(ctx, paidResult(invoiceOf(first.URL)))
	if err != nil || ap.Outcome != ApplyDoublePayment {
		t.Fatalf("first page: %v %v", ap.Outcome, err)
	}
	var step string
	e.s.Pool.QueryRow(ctx, `SELECT last_error_step FROM orders WHERE id = $1`, e.orderID).Scan(&step)
	if step != "double_payment" {
		t.Errorf("last_error_step %q", step)
	}
}

func TestApplyRejectsWrongAmount(t *testing.T) {
	e := newTestEnv(t)
	res, _ := e.pay(t)
	r := paidResult(invoiceOf(res.URL))
	r.AmountCents = 100
	if ap, err := e.s.Apply(context.Background(), r); err != nil || ap.Outcome != ApplyMismatch {
		t.Fatalf("got %v %v", ap.Outcome, err)
	}
	if s := e.orderStatus(t); s != "pending_payment" {
		t.Errorf("order status %q", s)
	}
}

func TestFailedNotificationAllowsOneRetry(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	first, _ := e.pay(t)
	r := paidResult(invoiceOf(first.URL))
	r.Status, r.RespCode, r.RespDesc = ResultFailed, "4005", "Declined"
	if ap, _ := e.s.Apply(ctx, r); ap.Outcome != ApplyFailed {
		t.Fatalf("failed result: %v", ap.Outcome)
	}
	next, err := e.pay(t)
	if err != nil || next.Outcome != PayNewLink {
		t.Fatalf("retry: %+v %v", next, err)
	}
}

func TestSettleOnReturn(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	res, _ := e.pay(t)
	inv := invoiceOf(res.URL)

	if st, _ := e.s.Settle(ctx, "FB999T1R0000"); st.Status != SettleUnknownInvoice {
		t.Errorf("unknown invoice: %v", st.Status)
	}

	// 2C2P unreachable, then still in progress: wait.
	for _, ans := range []map[string]any{nil, {"respCode": "2001"}} {
		e.fake.inquiry = ans
		if st, err := e.s.Settle(ctx, inv); err != nil || st.Status != SettlePending {
			t.Errorf("inquiry %v: %v %v", ans, st.Status, err)
		}
	}

	// A failure is shown but not recorded: the page may still take payment.
	e.fake.inquiry = map[string]any{"respCode": "4081", "respDesc": "Unable to authenticate card holder"}
	st, err := e.s.Settle(ctx, inv)
	if err != nil || st.Status != SettleNotPaid || st.Applied.Outcome != ApplyNone {
		t.Fatalf("failed payment: %+v %v", st, err)
	}
	var status string
	e.s.Pool.QueryRow(ctx, `SELECT status FROM payment_attempts WHERE invoice_no = $1`, inv).Scan(&status)
	if status != "pending" {
		t.Errorf("attempt %q after a failed return, want pending", status)
	}

	// Paid: recorded once, with the order details for the page.
	e.fake.inquiry = map[string]any{"respCode": "0000", "amount": 2423.54, "currencyCode": "THB", "fxAmount": 100.0, "fxCurrencyCode": "SGD", "tranRef": "T1"}
	st, err = e.s.Settle(ctx, inv)
	if err != nil || st.Status != SettlePaid || st.Applied.Outcome != ApplyPaid || st.OrderID != e.orderID || st.PageFBID != "1" || st.AmountCents != 10000 {
		t.Fatalf("paid: %+v %v", st, err)
	}
	if s := e.orderStatus(t); s != "paid" {
		t.Errorf("order status %q", s)
	}
	// Coming back again (refresh): still paid, nothing applied again.
	e.fake.inquiry = nil
	if st, _ := e.s.Settle(ctx, inv); st.Status != SettlePaid || st.Applied.Outcome != ApplyNone {
		t.Errorf("second return: %+v", st)
	}
}
