// Package twoc2pwebhook receives 2C2P's backend payment notifications. The
// customer's browser comes back to the common payment return page
// (internal/paymentreturn).
package twoc2pwebhook

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"fb-order-bot/internal/db"
	"fb-order-bot/internal/notify"
	"fb-order-bot/internal/obs"
	"fb-order-bot/internal/payments"
	"fb-order-bot/internal/twoc2p"
)

// Handler handles the notification 2C2P posts to the backend return URL:
// {"payload": "<JWT>"}, signed with the merchant secret key.
type Handler struct {
	Pool     *pgxpool.Pool
	Client   *twoc2p.Client
	Payments *payments.Service
	Notify   *notify.Customer
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
	log = log.With("order_id", applied.OrderID)
	switch applied.Outcome {
	case payments.ApplyPaid:
		h.Notify.Paid(ctx, log, applied)
	case payments.ApplyFailed:
		h.Notify.Failed(ctx, log, applied, "2C2P Payment")
	}
	w.WriteHeader(http.StatusOK)
}
