package payments

import (
	"context"
	"errors"
	"regexp"
	"testing"
)

func TestReceiptURL(t *testing.T) {
	if got := ReceiptURL("https://shop.example/", "abc"); got != "https://shop.example/receipts/abc" {
		t.Errorf("got %q", got)
	}
	if ReceiptURL("", "abc") != "" || ReceiptURL("https://shop.example", "") != "" {
		t.Error("want empty without base or token")
	}
}

func TestFormatMoney(t *testing.T) {
	cases := map[int64]string{10000: "SGD 100", 10050: "SGD 100.50", 5: "SGD 0.05"}
	for cents, want := range cases {
		if got := FormatMoney(cents, "sgd"); got != want {
			t.Errorf("FormatMoney(%d) = %q", cents, got)
		}
	}
}

func TestReceiptForPaidOrder(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.s.SetQuantity(ctx, e.orderID, 2)
	link, _ := e.payAmount(t, 20000)
	inv := invoiceOf(link.URL)

	r := paidResult(inv)
	r.AmountCents, r.ChannelCode = 20000, "VI"
	ap, err := e.s.Apply(ctx, r)
	if err != nil || ap.Outcome != ApplyPaid || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(ap.ReceiptToken) {
		t.Fatalf("apply: %+v %v", ap, err)
	}

	rc, err := e.s.Receipt(ctx, ap.ReceiptToken)
	if err != nil {
		t.Fatal(err)
	}
	if rc.OrderID != e.orderID || rc.ShopName != "Shop" || rc.Quantity != 2 || rc.UnitPriceCents == nil || *rc.UnitPriceCents != 10000 ||
		rc.AmountCents != 20000 || rc.Method != Method2C2P || rc.ChannelCode != "VI" || rc.TranRef != "T-"+inv || rc.InvoiceNo != inv || rc.PaidAt.IsZero() {
		t.Errorf("receipt %+v", rc)
	}

	// Coming back from the payment page later shows the same receipt.
	if st, _ := e.s.Settle(ctx, inv); st.Status != SettlePaid || st.ReceiptToken != ap.ReceiptToken {
		t.Errorf("settle: %+v", st)
	}
	if _, err := e.s.Receipt(ctx, "0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrNoReceipt) {
		t.Errorf("unknown token: %v", err)
	}
}

func TestUnpaidOrderHasNoReceipt(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	e.pay(t)
	var token *string
	e.s.Pool.QueryRow(ctx, `SELECT receipt_token FROM orders WHERE id = $1`, e.orderID).Scan(&token)
	if token != nil {
		t.Errorf("unpaid order has receipt token %q", *token)
	}
}
