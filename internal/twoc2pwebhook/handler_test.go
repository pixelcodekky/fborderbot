package twoc2pwebhook

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"fb-order-bot/internal/twoc2p"
	"fb-order-bot/web"
)

func TestRejectsUnsignedNotification(t *testing.T) {
	// Nil Pool: a bad signature must be rejected before any database access.
	h := &Handler{Client: twoc2p.New("", "M1", "secret", "", "")}
	for _, body := range []string{`{}`, `not json`, `{"payload":"a.b.c"}`} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhooks/2c2p", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status %d", body, rec.Code)
		}
	}
}

func TestReturnedInvoiceNo(t *testing.T) {
	// As 2C2P posts it: base64 JSON in the paymentResponse form field.
	resp := base64.StdEncoding.EncodeToString([]byte(`{"invoiceNo":"FB15T1792623842723R84E3","respCode":"2000"}`))
	req := httptest.NewRequest(http.MethodPost, "/payments/2c2p/return", strings.NewReader("paymentResponse="+url.QueryEscape(resp)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := returnedInvoiceNo(req); got != "FB15T1792623842723R84E3" {
		t.Errorf("POST: %q", got)
	}
	for _, q := range []string{"", "?paymentResponse=notbase64!!", "?paymentResponse=" + base64.StdEncoding.EncodeToString([]byte("[]"))} {
		if got := returnedInvoiceNo(httptest.NewRequest(http.MethodGet, "/payments/2c2p/return"+q, nil)); got != "" {
			t.Errorf("%q: %q", q, got)
		}
	}
}

func TestReturnPageWithoutInvoice(t *testing.T) {
	// Nil Payments: without an invoice the page renders without any lookup.
	rec := httptest.NewRecorder()
	(&Handler{}).ReturnPage(rec, httptest.NewRequest(http.MethodGet, "/payments/2c2p/return", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "return to Messenger") {
		t.Errorf("status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestPaymentReturnTemplate(t *testing.T) {
	cases := map[string]string{
		web.StatusPaid:    "Payment received",
		web.StatusFailed:  "Payment not completed",
		web.StatusPending: "confirming your payment",
		web.StatusUnknown: "return to Messenger",
	}
	for status, want := range cases {
		var b strings.Builder
		data := web.PaymentReturnData{Status: status, OrderID: 16, Total: "SGD 100", MessengerURL: "https://m.me/1267866303069580"}
		if err := web.PaymentReturn.Execute(&b, data); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(b.String(), want) {
			t.Errorf("%s: missing %q", status, want)
		}
		if status != web.StatusUnknown && !strings.Contains(b.String(), "Order #16 · SGD 100") {
			t.Errorf("%s: missing order line", status)
		}
	}
}
