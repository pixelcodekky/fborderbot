package receipt

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fb-order-bot/internal/payments"
	"fb-order-bot/web"
)

func TestBadTokenIsNotFound(t *testing.T) {
	// Nil Payments: a malformed token must 404 before any lookup.
	mux := http.NewServeMux()
	mux.Handle("GET /receipts/{token}", &Handler{})
	for _, path := range []string{"/receipts/abc", "/receipts/0123456789ABCDEF0123456789ABCDEF", "/receipts/' OR 1=1 --"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, strings.ReplaceAll(path, " ", "%20"), nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", path, rec.Code)
		}
	}
}

func TestPaidWith(t *testing.T) {
	cases := map[[2]string]string{
		{"2c2p", "VI"}:       "2C2P · Visa",
		{"2c2p", "XX"}:       "2C2P · XX",
		{"2c2p", ""}:         "2C2P",
		{"paynow", "paynow"}: "PayNow",
		{"stripe", "card"}:   "Stripe · Card",
		{"stripe", "paynow"}: "Stripe · PayNow",
		{"stripe", ""}:       "Stripe",
	}
	for in, want := range cases {
		if got := paidWith(in[0], in[1]); got != want {
			t.Errorf("paidWith%v = %q, want %q", in, got, want)
		}
	}
}

func TestReceiptPage(t *testing.T) {
	unit := int64(10000)
	sgt, _ := time.LoadLocation("Asia/Singapore")
	h := &Handler{Location: sgt}
	d := h.data(payments.Receipt{
		OrderID: 19, ShopName: "Test Shop", CustomerName: "Kyaw", ProductName: "test sales", Quantity: 2,
		UnitPriceCents: &unit, AmountCents: 20000, Currency: "sgd", Method: "2c2p", ChannelCode: "VI",
		TranRef: "15725984", InvoiceNo: "FB19T1792643479565RB9A5", PaidAt: time.Date(2026, 10, 22, 4, 31, 0, 0, time.UTC),
	})
	var b strings.Builder
	if err := web.Receipt.Execute(&b, d); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Test Shop", "#19", "test sales", "SGD 100", "SGD 200", "22 Oct 2026, 12:31 pm", "2C2P · Visa", "15725984", "FB19T1792643479565RB9A5", "Kyaw"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("receipt page lacks %q", want)
		}
	}
}
