package payments

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Receipt is what a paid order's receipt page shows.
type Receipt struct {
	OrderID        int64
	ShopName       string
	CustomerName   string // commenter's name; may be empty
	ProductName    string // may be empty
	Quantity       int
	UnitPriceCents *int64
	AmountCents    int64
	Currency       string
	Method         string // payment_attempts.provider
	ChannelCode    string // e.g. "VI", "paynow"
	TranRef        string // the provider's payment id
	InvoiceNo      string
	PaidAt         time.Time
}

// ErrNoReceipt means no paid order has that receipt token.
var ErrNoReceipt = errors.New("no receipt with that token")

// Receipt loads the paid order a receipt token belongs to.
func (s *Service) Receipt(ctx context.Context, token string) (Receipt, error) {
	var r Receipt
	var customer, product, channel, tranRef *string
	err := s.Pool.QueryRow(ctx,
		`SELECT o.id, p.name, o.commenter_name, pr.name, o.quantity, o.unit_price_cents,
		        a.amount_cents, a.currency, a.provider, a.channel_code, a.tran_ref, a.invoice_no, a.updated_at
		 FROM orders o
		 JOIN pages p ON p.id = o.page_id
		 LEFT JOIN products pr ON pr.id = o.product_id
		 JOIN LATERAL (SELECT * FROM payment_attempts
		               WHERE order_id = o.id AND status = 'succeeded'
		               ORDER BY id LIMIT 1) a ON true
		 WHERE o.receipt_token = $1`,
		token,
	).Scan(&r.OrderID, &r.ShopName, &customer, &product, &r.Quantity, &r.UnitPriceCents,
		&r.AmountCents, &r.Currency, &r.Method, &channel, &tranRef, &r.InvoiceNo, &r.PaidAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNoReceipt
	}
	if err != nil {
		return r, err
	}
	for dst, src := range map[*string]*string{&r.CustomerName: customer, &r.ProductName: product, &r.ChannelCode: channel, &r.TranRef: tranRef} {
		if src != nil {
			*dst = *src
		}
	}
	return r, nil
}

// ReceiptURL is the receipt page's link under the bot's public base URL
// (e.g. "https://shop.example"). Empty when either is unknown.
func ReceiptURL(baseURL, token string) string {
	if baseURL == "" || token == "" {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + "/receipts/" + url.PathEscape(token)
}

// FormatMoney renders 10000 "sgd" as "SGD 100" and 10050 as "SGD 100.50".
func FormatMoney(cents int64, currency string) string {
	cur := strings.ToUpper(currency)
	if cents%100 == 0 {
		return fmt.Sprintf("%s %d", cur, cents/100)
	}
	return fmt.Sprintf("%s %d.%02d", cur, cents/100, cents%100)
}
