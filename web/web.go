// Package web holds the pages the bot serves to customers' browsers. They are
// embedded, so the binary needs no files next to it.
package web

import (
	"embed"
	"html/template"
)

//go:embed pages/*.html
var pages embed.FS

// PaymentReturn is shown when 2C2P sends the customer back after the payment
// page. Its data is PaymentReturnData.
var PaymentReturn = template.Must(template.ParseFS(pages, "pages/payment_return.html"))

// Payment return states.
const (
	StatusPaid    = "paid"
	StatusFailed  = "failed"
	StatusPending = "pending"
	StatusUnknown = "unknown"
)

type PaymentReturnData struct {
	Status       string // one of the Status constants
	OrderID      int64  // 0 when unknown
	Total        string // e.g. "SGD 100"; empty when unknown
	MessengerURL string // link back to the Page's chat; empty when unknown
}
