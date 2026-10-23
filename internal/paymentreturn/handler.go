// Package paymentreturn is the one page every payment method sends the
// customer's browser back to, paid, failed or cancelled.
package paymentreturn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"

	"fb-order-bot/internal/notify"
	"fb-order-bot/internal/obs"
	"fb-order-bot/internal/payments"
	"fb-order-bot/web"
)

// Handler serves the payment return page. It accepts two kinds of return:
//
//   - Stripe Checkout: ?invoice=&result=&sig= from payments.ReturnLink.
//     result=cancel closes the Checkout Session, so the customer can pick
//     another method at once; that is why the link must be signed.
//   - 2C2P: {"invoiceNo", "respCode", ...} base64 in the paymentResponse
//     form field. That field isn't signed, so only its invoiceNo is used.
//
// Either way the result comes from the provider (payments.Service.Settle),
// so the order doesn't depend on its notification arriving.
type Handler struct {
	Payments  *payments.Service
	ReturnKey []byte // what Stripe's return links are signed with
	Notify    *notify.Customer
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithoutCancel(r.Context())
	log := obs.Logger(ctx).With("source", "payment_return")
	data := web.PaymentReturnData{Status: web.StatusUnknown}

	invoiceNo, cancel, ok := h.returned(r)
	switch {
	case !ok:
		log.Warn("return link with a bad signature", "invoice_no", invoiceNo)
	case invoiceNo != "":
		log = log.With("invoice_no", invoiceNo, "cancel", cancel)
		h.settle(ctx, log, invoiceNo, cancel, &data)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := web.PaymentReturn.Execute(w, data); err != nil {
		log.Error("render payment return page failed", "err", err)
	}
}

func (h *Handler) settle(ctx context.Context, log *slog.Logger, invoiceNo string, cancel bool, data *web.PaymentReturnData) {
	settle := h.Payments.Settle
	if cancel {
		settle = h.Payments.Cancel
	}
	st, err := settle(ctx, invoiceNo)
	if err != nil {
		log.Error("settle on return failed", "err", err)
		data.Status = web.StatusPending
		return
	}
	if st.Status == payments.SettleUnknownInvoice {
		log.Warn("customer returned with an unknown invoice")
		return
	}
	data.OrderID = st.OrderID
	data.Total = payments.FormatMoney(st.AmountCents, st.Currency)
	data.MessengerURL = "https://m.me/" + url.PathEscape(st.PageFBID)
	switch st.Status {
	case payments.SettlePaid:
		data.Status = web.StatusPaid
		if h.Notify != nil {
			data.ReceiptURL = payments.ReceiptURL(h.Notify.PublicBaseURL, st.ReceiptToken)
		}
	case payments.SettleCancelled:
		data.Status = web.StatusCancelled
	case payments.SettleNotPaid:
		data.Status = web.StatusFailed
	default:
		data.Status = web.StatusPending
	}
	log.Info("customer returned from payment page", "order_id", st.OrderID, "result", data.Status)
	// The notification may never come, so confirm here; Apply reports
	// ApplyPaid only once per payment, so it isn't sent twice.
	if st.Applied.Outcome == payments.ApplyPaid && h.Notify != nil {
		h.Notify.Paid(ctx, log.With("order_id", st.OrderID), st.Applied)
	}
}

// returned reads which invoice the customer came back from, and whether
// they cancelled. ok is false for a signed link whose signature is wrong.
// invoiceNo is empty when the request names none.
func (h *Handler) returned(r *http.Request) (invoiceNo string, cancel, ok bool) {
	if inv := r.FormValue("invoice"); inv != "" {
		result := r.FormValue("result")
		if len(inv) > 50 || !payments.VerifyReturn(h.ReturnKey, inv, result, r.FormValue("sig")) {
			return inv, false, false
		}
		return inv, result == payments.ReturnCancel, true
	}
	return twoC2PInvoiceNo(r), false, true
}

// twoC2PInvoiceNo reads the invoiceNo from 2C2P's paymentResponse (POST form
// or query string). Empty when absent or malformed.
func twoC2PInvoiceNo(r *http.Request) string {
	raw := r.FormValue("paymentResponse")
	if raw == "" {
		return ""
	}
	var decoded []byte
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(raw); err == nil {
			decoded = b
			break
		}
	}
	var resp struct {
		InvoiceNo string `json:"invoiceNo"`
	}
	if json.Unmarshal(decoded, &resp) != nil || len(resp.InvoiceNo) > 50 {
		return ""
	}
	return resp.InvoiceNo
}
