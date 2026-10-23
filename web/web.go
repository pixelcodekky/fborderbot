// Package web holds the pages the bot serves to customers' browsers. They are
// embedded, so the binary needs no files next to it.
package web

import (
	"embed"
	"html/template"
)

//go:embed pages/*.html
var pages embed.FS

// PaymentReturn is shown when any payment method sends the customer back
// after its payment page. Its data is PaymentReturnData.
var PaymentReturn = template.Must(template.ParseFS(pages, "pages/payment_return.html"))

// Payment return states.
const (
	StatusPaid      = "paid"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled" // left the payment page; nothing charged
	StatusPending   = "pending"
	StatusUnknown   = "unknown"
)

type PaymentReturnData struct {
	Status       string // one of the Status constants
	OrderID      int64  // 0 when unknown
	Total        string // e.g. "SGD 100"; empty when unknown
	MessengerURL string // link back to the Page's chat; empty when unknown
	ReceiptURL   string // StatusPaid: the order's receipt; empty when unknown
}

// Receipt is a paid order's receipt. Its data is ReceiptData.
var Receipt = template.Must(template.ParseFS(pages, "pages/receipt.html"))

// ReceiptData holds display-ready strings.
type ReceiptData struct {
	ShopName     string
	OrderID      int64
	Item         string // empty when the order has no product
	Quantity     int
	UnitPrice    string // e.g. "SGD 100"; empty when unknown
	Total        string
	CustomerName string
	PaidAt       string // e.g. "22 Oct 2026, 12:31 pm"
	PaidWith     string // e.g. "2C2P · Visa"
	Reference    string // the provider's payment id
	InvoiceNo    string
}
