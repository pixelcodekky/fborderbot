package fbwebhook

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"fb-order-bot/internal/messenger"
	"fb-order-bot/internal/payments"
)

// Payment methods offered as buttons in the comment reply.
const (
	method2C2P   = "2c2p"
	methodStripe = "stripe" // Stripe Checkout: card and PayNow
	// methodPayNow is the old PayNow button, still in customers' earlier
	// messages; a tap on it is a tap on Stripe Payment.
	methodPayNow = "paynow"
)

// keywordProduct is the product a trigger keyword sells.
type keywordProduct struct {
	keyword    string // as stored in trigger_keywords
	productID  int64
	name       string
	priceCents int64
	currency   string
}

// lookupProduct returns the product linked to the first of matched (in order)
// that has an active product. It returns pgx.ErrNoRows when none has one.
func (h *Handler) lookupProduct(ctx context.Context, pageID int64, matched []string) (p keywordProduct, err error) {
	lower := make([]string, len(matched))
	for i, k := range matched {
		lower[i] = strings.ToLower(k)
	}
	err = h.Pool.QueryRow(ctx,
		`SELECT k.keyword, pr.id, pr.name, pr.price_cents, pr.currency
		 FROM trigger_keywords k
		 JOIN products pr ON pr.id = k.product_id AND pr.active
		 WHERE k.page_id = $1 AND k.active AND lower(k.keyword) = ANY($2::text[])
		 ORDER BY array_position($2::text[], lower(k.keyword))
		 LIMIT 1`,
		pageID, lower,
	).Scan(&p.keyword, &p.productID, &p.name, &p.priceCents, &p.currency)
	return
}

// quantityPromptText is the comment reply, above the quantity buttons:
//
//	Order: test sales
//	Price: SGD 100 each
//	How many would you like?
func quantityPromptText(keyword string, priceCents int64, currency string) string {
	return fmt.Sprintf("Order: %s\nPrice: %s each\nHow many would you like?", keyword, formatMoney(priceCents, currency))
}

// orderReplyText is the text above the payment buttons, once the quantity is
// chosen:
//
//	Order: test sales x 2
//	Total: SGD 200
//	Choose Payment
func orderReplyText(name string, qty int, amountCents int64, currency string) string {
	return fmt.Sprintf("Order: %s x %d\nTotal: %s\nChoose Payment", name, qty, formatMoney(amountCents, currency))
}

// formatMoney renders 10000 "sgd" as "SGD 100" and 10050 as "SGD 100.50".
func formatMoney(cents int64, currency string) string {
	cur := strings.ToUpper(currency)
	if cents%100 == 0 {
		return fmt.Sprintf("%s %d", cur, cents/100)
	}
	return fmt.Sprintf("%s %d.%02d", cur, cents/100, cents%100)
}

func paymentButtons(orderID int64) []messenger.Button {
	return []messenger.Button{
		{Type: "postback", Title: "2C2P Payment", Payload: payPayload(orderID, method2C2P)},
		{Type: "postback", Title: "Stripe Payment", Payload: payPayload(orderID, methodStripe)},
		{Type: "postback", Title: "Change quantity", Payload: qtyPayload(orderID, 0)},
	}
}

// quantityButtons go under the comment reply; "3+" asks with quantityChoices.
func quantityButtons(orderID int64) []messenger.Button {
	return []messenger.Button{
		{Type: "postback", Title: "1", Payload: qtyPayload(orderID, 1)},
		{Type: "postback", Title: "2", Payload: qtyPayload(orderID, 2)},
		{Type: "postback", Title: "3+", Payload: qtyPayload(orderID, 0)},
	}
}

// quantityChoices are quick replies 1 to payments.MaxQuantity.
func quantityChoices(orderID int64) []messenger.QuickReply {
	out := make([]messenger.QuickReply, payments.MaxQuantity)
	for i := range out {
		out[i] = messenger.QuickReply{Title: strconv.Itoa(i + 1), Payload: qtyPayload(orderID, i+1)}
	}
	return out
}

// Quantity payloads look like "QTY:42:3"; "QTY:42:more" asks for one.
func qtyPayload(orderID int64, qty int) string {
	q := "more"
	if qty > 0 {
		q = strconv.Itoa(qty)
	}
	return "QTY:" + strconv.FormatInt(orderID, 10) + ":" + q
}

// parseQtyPayload returns qty 0 for "more". A number out of range is
// returned as is, for the caller to explain.
func parseQtyPayload(payload string) (orderID int64, qty int, ok bool) {
	parts := strings.Split(payload, ":")
	if len(parts) != 3 || parts[0] != "QTY" {
		return 0, 0, false
	}
	orderID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || orderID <= 0 {
		return 0, 0, false
	}
	if parts[2] == "more" {
		return orderID, 0, true
	}
	qty, ok = parseQuantity(parts[2])
	return orderID, qty, ok
}

// parseQuantity reads a typed quantity: digits only, e.g. "3" or " 12 ".
func parseQuantity(text string) (int, bool) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 3 {
		return 0, false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(text)
	return n, err == nil
}

// Postback payloads look like "PAY:42:stripe".
func payPayload(orderID int64, method string) string {
	return "PAY:" + strconv.FormatInt(orderID, 10) + ":" + method
}

func parsePayPayload(payload string) (orderID int64, method string, ok bool) {
	parts := strings.Split(payload, ":")
	if len(parts) != 3 || parts[0] != "PAY" {
		return 0, "", false
	}
	orderID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || orderID <= 0 {
		return 0, "", false
	}
	switch parts[2] {
	case method2C2P, methodStripe:
		return orderID, parts[2], true
	case methodPayNow:
		return orderID, methodStripe, true
	}
	return 0, "", false
}
