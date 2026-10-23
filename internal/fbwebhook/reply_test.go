package fbwebhook

import (
	"context"
	"strconv"
	"testing"

	"fb-order-bot/internal/payments"
)

func TestReplyTexts(t *testing.T) {
	if got, want := quantityPromptText("test sales", 10000, "sgd"), "Order: test sales\nPrice: SGD 100 each\nHow many would you like?"; got != want {
		t.Errorf("quantity prompt %q, want %q", got, want)
	}
	if got, want := orderReplyText("test sales", 2, 20000, "sgd"), "Order: test sales x 2\nTotal: SGD 200\nChoose Payment"; got != want {
		t.Errorf("order reply %q, want %q", got, want)
	}
}

func TestFormatMoney(t *testing.T) {
	cases := map[int64]string{10000: "SGD 100", 10050: "SGD 100.50", 5: "SGD 0.05", 0: "SGD 0"}
	for cents, want := range cases {
		if got := formatMoney(cents, "sgd"); got != want {
			t.Errorf("formatMoney(%d) = %q, want %q", cents, got, want)
		}
	}
}

func TestPaymentButtons(t *testing.T) {
	b := paymentButtons(42)
	if len(b) != 3 || b[0].Title != "2C2P Payment" || b[1].Title != "Stripe Payment" || b[2].Title != "Change quantity" {
		t.Fatalf("buttons: %+v", b)
	}
	wantMethods := []string{method2C2P, methodStripe}
	for i, btn := range b[:2] {
		id, method, ok := parsePayPayload(btn.Payload)
		if btn.Type != "postback" || !ok || id != 42 || method != wantMethods[i] {
			t.Errorf("%s: %q payload %q parsed as %d %q %v", btn.Title, btn.Type, btn.Payload, id, method, ok)
		}
	}
	if id, qty, ok := parseQtyPayload(b[2].Payload); !ok || id != 42 || qty != 0 {
		t.Errorf("change quantity payload %q", b[2].Payload)
	}
}

func TestQuantityButtonsAndChoices(t *testing.T) {
	want := []int{1, 2, 0}
	for i, btn := range quantityButtons(42) {
		if id, qty, ok := parseQtyPayload(btn.Payload); !ok || id != 42 || qty != want[i] {
			t.Errorf("%s: payload %q", btn.Title, btn.Payload)
		}
	}
	c := quantityChoices(42)
	if len(c) != payments.MaxQuantity || len(c) > 13 {
		t.Fatalf("%d quick replies", len(c))
	}
	for i, q := range c {
		if id, qty, ok := parseQtyPayload(q.Payload); !ok || id != 42 || qty != i+1 || q.Title != strconv.Itoa(i+1) {
			t.Errorf("choice %+v", q)
		}
	}
}

func TestParseQtyPayload(t *testing.T) {
	for _, bad := range []string{"", "QTY:42", "QTY:x:1", "QTY:0:1", "QTY:42:-1", "QTY:42:two", "QTY:42:1:x", "PAY:42:paynow"} {
		if _, _, ok := parseQtyPayload(bad); ok {
			t.Errorf("parseQtyPayload(%q) accepted", bad)
		}
	}
	// Out of range parses; the handler explains the limit.
	if id, qty, ok := parseQtyPayload("QTY:7:15"); !ok || id != 7 || qty != 15 {
		t.Errorf("QTY:7:15 -> %d %d %v", id, qty, ok)
	}
}

func TestParseQuantity(t *testing.T) {
	good := map[string]int{"1": 1, " 3 ": 3, "10": 10, "15": 15, "0": 0}
	for in, want := range good {
		if n, ok := parseQuantity(in); !ok || n != want {
			t.Errorf("parseQuantity(%q) = %d %v", in, n, ok)
		}
	}
	for _, bad := range []string{"", "two", "2 pcs", "-1", "1.5", "91234567", "+65"} {
		if _, ok := parseQuantity(bad); ok {
			t.Errorf("parseQuantity(%q) accepted", bad)
		}
	}
}

func TestParsePayPayload(t *testing.T) {
	for _, bad := range []string{"", "PAY:42", "PAY:x:paynow", "PAY:0:paynow", "PAY:42:card", "GET_STARTED", "PAY:42:paynow:x"} {
		if _, _, ok := parsePayPayload(bad); ok {
			t.Errorf("parsePayPayload(%q) accepted", bad)
		}
	}
	if id, m, ok := parsePayPayload("PAY:7:2c2p"); !ok || id != 7 || m != method2C2P {
		t.Errorf("PAY:7:2c2p -> %d %q %v", id, m, ok)
	}
	// The old PayNow button, in earlier messages, now opens Stripe Checkout.
	if id, m, ok := parsePayPayload("PAY:7:paynow"); !ok || id != 7 || m != methodStripe {
		t.Errorf("PAY:7:paynow -> %d %q %v", id, m, ok)
	}
}

func TestProcessPostbackIgnoresUnknownPayload(t *testing.T) {
	// Nil Pool: an unknown payload must return before any database access.
	h := &Handler{}
	ev := messagingEvent{}
	ev.Postback = &struct {
		MID     string `json:"mid"`
		Title   string `json:"title"`
		Payload string `json:"payload"`
	}{MID: "m1", Payload: "GET_STARTED"}
	if h.processPostback(context.Background(), "1", ev) {
		t.Error("unexpected retry")
	}
}

func TestProcessMessageIgnoresEchoAndChat(t *testing.T) {
	// Nil Pool: echoes and ordinary chat must return before any database access.
	h := &Handler{}
	for _, m := range []struct {
		text string
		echo bool
	}{{"2", true}, {"hello, is this available?", false}, {"2 pcs", false}} {
		ev := messagingEvent{}
		ev.Message = &struct {
			MID        string `json:"mid"`
			Text       string `json:"text"`
			IsEcho     bool   `json:"is_echo"`
			QuickReply *struct {
				Payload string `json:"payload"`
			} `json:"quick_reply"`
		}{MID: "m1", Text: m.text, IsEcho: m.echo}
		if h.processMessage(context.Background(), "1", ev) {
			t.Errorf("%q: unexpected retry", m.text)
		}
	}
}
