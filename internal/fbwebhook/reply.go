package fbwebhook

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"fb-order-bot/internal/messenger"
)

// Payment methods offered as buttons in the comment reply.
const (
	method2C2P   = "2c2p"
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

// orderReplyText is the text above the payment buttons:
//
//	Order: test sales
//	Total: SGD 100
//	Choose Payment
func orderReplyText(keyword string, amountCents int64, currency string) string {
	return fmt.Sprintf("Order: %s\nTotal: %s\nChoose Payment", keyword, formatMoney(amountCents, currency))
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
		{Type: "postback", Title: "PayNow", Payload: payPayload(orderID, methodPayNow)},
	}
}

// Postback payloads look like "PAY:42:paynow".
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
	case method2C2P, methodPayNow:
		return orderID, parts[2], true
	}
	return 0, "", false
}
