package paymentreturn

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"fb-order-bot/internal/payments"
	"fb-order-bot/web"
)

func TestTwoC2PInvoiceNo(t *testing.T) {
	// As 2C2P posts it: base64 JSON in the paymentResponse form field.
	resp := base64.StdEncoding.EncodeToString([]byte(`{"invoiceNo":"FB15T1792623842723R84E3","respCode":"2000"}`))
	req := httptest.NewRequest(http.MethodPost, "/payments/return", strings.NewReader("paymentResponse="+url.QueryEscape(resp)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := twoC2PInvoiceNo(req); got != "FB15T1792623842723R84E3" {
		t.Errorf("POST: %q", got)
	}
	for _, q := range []string{"", "?paymentResponse=notbase64!!", "?paymentResponse=" + base64.StdEncoding.EncodeToString([]byte("[]"))} {
		if got := twoC2PInvoiceNo(httptest.NewRequest(http.MethodGet, "/payments/return"+q, nil)); got != "" {
			t.Errorf("%q: %q", q, got)
		}
	}
}

func TestReturnedStripeLink(t *testing.T) {
	key := []byte("k1")
	h := &Handler{ReturnKey: key}
	path := func(link string) string { u, _ := url.Parse(link); return u.RequestURI() }

	for result, wantCancel := range map[string]bool{payments.ReturnDone: false, payments.ReturnCancel: true} {
		link := payments.ReturnLink("https://bot.example/payments/return", key, "FB1T2R3", result)
		inv, cancel, ok := h.returned(httptest.NewRequest(http.MethodGet, path(link), nil))
		if !ok || inv != "FB1T2R3" || cancel != wantCancel {
			t.Errorf("%s: %q cancel=%v ok=%v", result, inv, cancel, ok)
		}
	}

	// A done link edited into a cancel, or signed with another key, is refused.
	done := path(payments.ReturnLink("/payments/return", key, "FB1T2R3", payments.ReturnDone))
	forged := strings.Replace(done, "result=done", "result=cancel", 1)
	other := path(payments.ReturnLink("/payments/return", []byte("k2"), "FB1T2R3", payments.ReturnCancel))
	for _, p := range []string{forged, other, "/payments/return?invoice=FB1T2R3&result=cancel"} {
		if _, _, ok := h.returned(httptest.NewRequest(http.MethodGet, p, nil)); ok {
			t.Errorf("%s accepted", p)
		}
	}
	if _, _, ok := (&Handler{}).returned(httptest.NewRequest(http.MethodGet, other, nil)); ok {
		t.Error("accepted with no key configured")
	}
}

func TestReturnLinkKeepsBaseQuery(t *testing.T) {
	link := payments.ReturnLink("https://bot.example/payments/return?shop=1", []byte("k"), "FB1", payments.ReturnDone)
	u, err := url.Parse(link)
	if err != nil || u.Query().Get("shop") != "1" || u.Query().Get("invoice") != "FB1" || u.Query().Get("sig") == "" {
		t.Errorf("link %s", link)
	}
}

func TestPageWithoutInvoice(t *testing.T) {
	// Nil Payments: without an invoice, or with a bad signature, the page
	// renders without any lookup.
	for _, p := range []string{"/payments/return", "/payments/return?invoice=FB1&result=cancel&sig=x"} {
		rec := httptest.NewRecorder()
		(&Handler{ReturnKey: []byte("k")}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "return to Messenger") {
			t.Errorf("%s: status %d body %s", p, rec.Code, rec.Body.String())
		}
	}
}

func TestPaymentReturnTemplate(t *testing.T) {
	cases := map[string]string{
		web.StatusPaid:      "Payment received",
		web.StatusFailed:    "Payment not completed",
		web.StatusCancelled: "Payment cancelled",
		web.StatusPending:   "confirming your payment",
		web.StatusUnknown:   "return to Messenger",
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
