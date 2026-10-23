// Package receipt serves paid orders' receipt pages, linked from the payment
// confirmation in Messenger. The link's token is the only key to a receipt.
package receipt

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"fb-order-bot/internal/obs"
	"fb-order-bot/internal/payments"
	"fb-order-bot/web"
)

type Handler struct {
	Payments *payments.Service
	Location *time.Location // for "Paid on"; defaults to UTC
}

// ServeHTTP handles GET /receipts/{token}.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := obs.Logger(r.Context()).With("source", "receipt")
	token := r.PathValue("token")
	if !validToken(token) {
		http.NotFound(w, r)
		return
	}
	rc, err := h.Payments.Receipt(r.Context(), token)
	if errors.Is(err, payments.ErrNoReceipt) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Error("load receipt failed", "err", err)
		http.Error(w, "Sorry, the receipt can't be shown right now. Please try again later.", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer") // the URL is the key
	if err := web.Receipt.Execute(w, h.data(rc)); err != nil {
		log.Error("render receipt failed", "order_id", rc.OrderID, "err", err)
	}
}

func (h *Handler) data(rc payments.Receipt) web.ReceiptData {
	loc := h.Location
	if loc == nil {
		loc = time.UTC
	}
	d := web.ReceiptData{
		ShopName:     rc.ShopName,
		OrderID:      rc.OrderID,
		Item:         rc.ProductName,
		Quantity:     rc.Quantity,
		Total:        payments.FormatMoney(rc.AmountCents, rc.Currency),
		CustomerName: rc.CustomerName,
		PaidAt:       rc.PaidAt.In(loc).Format("2 Jan 2006, 3:04 pm"),
		PaidWith:     paidWith(rc.Method, rc.ChannelCode),
		Reference:    rc.TranRef,
		InvoiceNo:    rc.InvoiceNo,
	}
	if rc.UnitPriceCents != nil {
		d.UnitPrice = payments.FormatMoney(*rc.UnitPriceCents, rc.Currency)
	}
	return d
}

// paidWith renders e.g. "2C2P · Visa", "Stripe · PayNow" or "PayNow".
func paidWith(method, channel string) string {
	switch method {
	case payments.MethodPayNow:
		return "PayNow"
	case payments.MethodStripe:
		switch channel {
		case "card":
			return "Stripe · Card"
		case "paynow":
			return "Stripe · PayNow"
		}
		return "Stripe"
	case payments.Method2C2P:
		if c := channelNames[strings.ToUpper(channel)]; c != "" {
			return "2C2P · " + c
		}
		if channel != "" {
			return "2C2P · " + channel
		}
		return "2C2P"
	}
	return method
}

// channelNames are 2C2P channel codes for card schemes.
var channelNames = map[string]string{
	"VI": "Visa",
	"MA": "Mastercard",
	"JC": "JCB",
	"AM": "American Express",
	"UP": "UnionPay",
}

// validToken accepts the 32 hex characters Apply makes, so other paths never
// reach the database.
func validToken(t string) bool {
	if len(t) != 32 {
		return false
	}
	for _, c := range t {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
