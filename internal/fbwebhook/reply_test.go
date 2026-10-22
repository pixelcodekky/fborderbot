package fbwebhook

import (
	"context"
	"testing"
)

func TestOrderReplyText(t *testing.T) {
	got := orderReplyText("test sales", 10000, "sgd")
	want := "Order: test sales\nTotal: SGD 100\nChoose Payment"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
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
	if len(b) != 2 || b[0].Title != "2C2P Payment" || b[1].Title != "PayNow" {
		t.Fatalf("buttons: %+v", b)
	}
	for _, btn := range b {
		if btn.Type != "postback" {
			t.Errorf("%s: type %q", btn.Title, btn.Type)
		}
		id, method, ok := parsePayPayload(btn.Payload)
		if !ok || id != 42 {
			t.Errorf("%s: payload %q parsed as %d %q %v", btn.Title, btn.Payload, id, method, ok)
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
