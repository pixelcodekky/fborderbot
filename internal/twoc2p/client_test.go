package twoc2p

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testSecret = "secret"

func TestCreatePaymentToken(t *testing.T) {
	var claims map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/payment/4.3/paymentToken" {
			t.Errorf("request to %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req struct{ Payload string }
		json.Unmarshal(body, &req)
		raw, err := decodeJWT(req.Payload, testSecret)
		if err != nil {
			t.Fatal(err)
		}
		json.Unmarshal(raw, &claims)
		resp, _ := encodeJWT(map[string]string{
			"webPaymentUrl": "https://pay.example/abc", "paymentToken": "tok", "respCode": "0000", "respDesc": "Success",
		}, testSecret)
		json.NewEncoder(w).Encode(map[string]string{"payload": resp})
	}))
	defer srv.Close()

	c := New(srv.URL, "M1", testSecret, "https://shop/return", "https://shop/webhooks/2c2p")
	url, err := c.CreatePaymentToken(context.Background(), PaymentTokenRequest{
		InvoiceNo: "FB1T2", Description: "Order #1", AmountCents: 10050, Currency: "sgd", CustomerName: "Kyaw",
	})
	if err != nil || url != "https://pay.example/abc" {
		t.Fatalf("url=%q err=%v", url, err)
	}
	for k, want := range map[string]any{
		"merchantID": "M1", "invoiceNo": "FB1T2", "amount": 100.5, "currencyCode": "SGD",
		"frontendReturnUrl": "https://shop/return", "backendReturnUrl": "https://shop/webhooks/2c2p",
	} {
		if claims[k] != want {
			t.Errorf("claim %s = %v, want %v", k, claims[k], want)
		}
	}
}

func TestCreatePaymentTokenError(t *testing.T) {
	// Errors come back as plain JSON, without a signed payload.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"respCode":"9042","respDesc":"Merchant not found."}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "M1", testSecret, "f", "b")
	_, err := c.CreatePaymentToken(context.Background(), PaymentTokenRequest{InvoiceNo: "FB1T2", AmountCents: 100, Currency: "sgd"})
	var re *RespError
	if !errors.As(err, &re) || re.RespCode != "9042" {
		t.Fatalf("err = %v", err)
	}
}

func TestParseNotification(t *testing.T) {
	c := New("", "M1", testSecret, "", "")
	payload, _ := encodeJWT(map[string]any{
		"merchantID": "M1", "invoiceNo": "FB1T2", "amount": 605.88, "currencyCode": "THB",
		"fxAmount": 25.00, "fxCurrencyCode": "SGD", "tranRef": "T1", "respCode": "0000",
	}, testSecret)
	n, err := c.ParseNotification(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !n.Paid() || n.TranRef != "T1" {
		t.Errorf("notification %+v", n)
	}
	if amount, cur := n.RequestedAmount(); amount != "25" || cur != "SGD" {
		t.Errorf("requested amount %s %s", amount, cur)
	}

	if _, err := New("", "M1", "other-secret", "", "").ParseNotification(payload); err == nil {
		t.Error("accepted a notification signed with another key")
	}
	if _, err := New("", "M2", testSecret, "", "").ParseNotification(payload); err == nil {
		t.Error("accepted another merchant's notification")
	}
}

func TestFormatAmount(t *testing.T) {
	cases := map[int64]string{10000: "100.00", 10050: "100.50", 5: "0.05"}
	for cents, want := range cases {
		if got := formatAmount(cents); got != want {
			t.Errorf("formatAmount(%d) = %q, want %q", cents, got, want)
		}
	}
}

func TestInquire(t *testing.T) {
	var signed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/payment/4.3/paymentInquiry" {
			t.Errorf("request to %s", r.URL.Path)
		}
		if !signed {
			w.Write([]byte(`{"respCode":"4200","respDesc":"Transaction not found"}`))
			return
		}
		resp, _ := encodeJWT(map[string]any{"merchantID": "M1", "invoiceNo": "FB1T2", "respCode": "0000", "amount": 100.00, "currencyCode": "SGD"}, testSecret)
		json.NewEncoder(w).Encode(map[string]string{"payload": resp})
	}))
	defer srv.Close()
	c := New(srv.URL, "M1", testSecret, "f", "b")

	// An unsigned answer is a RespError, never a result.
	_, err := c.Inquire(context.Background(), "FB1T2")
	var re *RespError
	if !errors.As(err, &re) || re.RespCode != "4200" {
		t.Fatalf("unsigned: %v", err)
	}

	signed = true
	r, err := c.Inquire(context.Background(), "FB1T2")
	if err != nil || !r.Paid() {
		t.Fatalf("signed: %+v %v", r, err)
	}
	if _, err := c.Inquire(context.Background(), "FB9T9"); err == nil {
		t.Error("accepted an answer for another invoice")
	}
}

func TestPaymentExpiryFormat(t *testing.T) {
	var claims map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct{ Payload string }
		json.Unmarshal(body, &req)
		raw, _ := decodeJWT(req.Payload, testSecret)
		json.Unmarshal(raw, &claims)
		w.Write([]byte(`{"respCode":"9999","respDesc":"x"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "M1", testSecret, "f", "b")
	c.CreatePaymentToken(context.Background(), PaymentTokenRequest{
		InvoiceNo: "FB1T2", AmountCents: 100, Currency: "sgd",
		ExpiresAt: time.Date(2026, 10, 22, 2, 20, 0, 0, time.UTC),
	})
	if got := claims["paymentExpiry"]; got != "2026-10-22 10:20:00" { // Singapore is UTC+8
		t.Errorf("paymentExpiry = %v", got)
	}
}
