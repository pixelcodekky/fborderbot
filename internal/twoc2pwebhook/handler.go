// Package twoc2pwebhook receives 2C2P's backend payment notifications and the
// customer's browser coming back from the 2C2P payment page.
package twoc2pwebhook

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"fb-order-bot/internal/db"
	"fb-order-bot/internal/messenger"
	"fb-order-bot/internal/obs"
	"fb-order-bot/internal/payments"
	"fb-order-bot/internal/twoc2p"
	"fb-order-bot/web"
)

// Handler handles the notification 2C2P posts to the backend return URL:
// {"payload": "<JWT>"}, signed with the merchant secret key.
type Handler struct {
	Pool         *pgxpool.Pool
	Client       *twoc2p.Client
	Payments     *payments.Service
	NewMessenger func(pageAccessToken string) *messenger.Client
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Detached from the request so a client hang-up can't abort half-done work.
	ctx := context.WithoutCancel(r.Context())
	log := obs.Logger(ctx).With("source", "2c2p")

	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Warn("read body failed", "err", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var req struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Payload == "" {
		log.Warn("notification has no payload")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	n, err := h.Client.ParseNotification(req.Payload)
	if err != nil {
		log.Warn("invalid notification; check TWOC2P_SECRET_KEY / TWOC2P_MERCHANT_ID", "err", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	log = log.With("invoice_no", n.InvoiceNo, "resp_code", n.RespCode, "tran_ref", n.TranRef)

	// Dedupe: 2C2P resends a notification it didn't get a 200 for. Apply is
	// safe to repeat; this only keeps the customer from a second message.
	eventID := n.InvoiceNo + ":" + n.RespCode
	first, err := db.MarkEventSeen(ctx, h.Pool, "2c2p", eventID)
	if err != nil {
		log.Error("dedupe check failed", "step", "dedupe", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !first {
		log.Info("duplicate notification skipped")
		w.WriteHeader(http.StatusOK)
		return
	}

	applied, err := h.Payments.Apply(ctx, payments.TwoC2PResult(n))
	if err != nil {
		// Release the claim and 500 so 2C2P's resend processes it again.
		log.Error("notification handling failed; 2C2P will resend", "err", err)
		if err := db.ReleaseEvent(ctx, h.Pool, "2c2p", eventID); err != nil {
			log.Error("release dedupe claim failed", "err", err)
		}
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	// The result is recorded from here on, so a failed message is recorded
	// on the order rather than retried (a retry could double-send).
	h.notifyCustomer(ctx, log.With("order_id", applied.OrderID), applied)
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) notifyCustomer(ctx context.Context, log *slog.Logger, ap payments.Applied) {
	var msg, step string
	switch ap.Outcome {
	case payments.ApplyPaid:
		step = "confirmation_message"
		msg = fmt.Sprintf("Payment received ✅ Order #%d for %s is confirmed. We'll be in touch about delivery!", ap.OrderID, ap.ProductName)
	case payments.ApplyFailed:
		step = "payment_failed_message"
		msg = fmt.Sprintf("Your payment for order #%d didn't go through. Please tap \"2C2P Payment\" again to retry.", ap.OrderID)
	default:
		return // nothing new for the customer; problems are already recorded
	}
	if ap.PSID == nil {
		log.Warn("order has no psid; skipping Messenger message", "step", step)
		return
	}
	if err := h.NewMessenger(ap.PageAccessToken).SendText(*ap.PSID, msg); err != nil {
		log.Error(step+" failed", "step", step, "err", err)
		if dbErr := db.RecordOrderError(ctx, h.Pool, ap.OrderID, step, err); dbErr != nil {
			log.Error("record order error failed", "err", dbErr)
		}
	}
}

// ReturnPage is the frontend return URL: 2C2P sends the customer's browser
// here after the payment page, with {"invoiceNo", "respCode", ...} base64 in
// the paymentResponse form field. That field isn't signed, so only its
// invoiceNo is used: the result comes from Payment Inquiry (see
// payments.Service.Settle), which also means the order no longer depends on
// the backend notification arriving.
func (h *Handler) ReturnPage(w http.ResponseWriter, r *http.Request) {
	ctx := context.WithoutCancel(r.Context())
	log := obs.Logger(ctx).With("source", "2c2p_return")
	data := web.PaymentReturnData{Status: web.StatusUnknown}

	if invoiceNo := returnedInvoiceNo(r); invoiceNo != "" {
		log = log.With("invoice_no", invoiceNo)
		st, err := h.Payments.Settle(ctx, invoiceNo)
		if err != nil {
			log.Error("settle on return failed", "err", err)
			data.Status = web.StatusPending
		} else if st.Status != payments.SettleUnknownInvoice {
			data.OrderID = st.OrderID
			data.Total = formatTotal(st.AmountCents, st.Currency)
			data.MessengerURL = "https://m.me/" + url.PathEscape(st.PageFBID)
			switch st.Status {
			case payments.SettlePaid:
				data.Status = web.StatusPaid
			case payments.SettleNotPaid:
				data.Status = web.StatusFailed
			default:
				data.Status = web.StatusPending
			}
			log.Info("customer returned from 2c2p", "order_id", st.OrderID, "result", data.Status)
			// The notification may never come, so confirm here; Apply
			// reports ApplyPaid only once per payment, so it isn't sent twice.
			h.notifyCustomer(ctx, log.With("order_id", st.OrderID), st.Applied)
		} else {
			log.Warn("customer returned with an unknown invoice")
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := web.PaymentReturn.Execute(w, data); err != nil {
		log.Error("render payment return page failed", "err", err)
	}
}

// returnedInvoiceNo reads the invoiceNo from 2C2P's paymentResponse (POST
// form or query string). Empty when absent or malformed.
func returnedInvoiceNo(r *http.Request) string {
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

// formatTotal renders 10000 "sgd" as "SGD 100" and 10050 as "SGD 100.50".
func formatTotal(cents int64, currency string) string {
	cur := strings.ToUpper(currency)
	if cents%100 == 0 {
		return fmt.Sprintf("%s %d", cur, cents/100)
	}
	return fmt.Sprintf("%s %d.%02d", cur, cents/100, cents%100)
}
