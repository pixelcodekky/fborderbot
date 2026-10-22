package fbwebhook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"fb-order-bot/internal/db"
	"fb-order-bot/internal/messenger"
	"fb-order-bot/internal/obs"
	"fb-order-bot/internal/payments"
)

// postbackOrder is what a payment button tap needs to know about its order.
type postbackOrder struct {
	status      string
	psid        *string
	amountCents *int64
	currency    string
	productName *string
	customer    *string // commenter's name
	accessToken string
}

// processPostback handles a tap on one of the comment reply's payment buttons
// (payload "PAY:<order id>:<method>"). PayNow makes a Stripe Checkout and
// 2C2P a 2C2P hosted payment page; either way the customer gets its link. An
// order gets one live link at a time, of either method, so it can't be paid
// twice.
// It reports retry like processComment, releasing its dedupe claim first.
func (h *Handler) processPostback(ctx context.Context, pageFBID string, ev messagingEvent) (retry bool) {
	log := obs.Logger(ctx).With(
		"source", "facebook",
		"page_fb_id", pageFBID,
		"psid", ev.Sender.ID,
		"payload", ev.Postback.Payload,
	)

	orderID, method, ok := parsePayPayload(ev.Postback.Payload)
	if !ok {
		log.Info("unknown postback payload; ignored")
		return false
	}
	log = log.With("order_id", orderID, "payment_method", method)

	// Dedupe on the postback's message id: a redelivery has the same mid, a
	// second tap gets a new one.
	release := func() {}
	if mid := ev.Postback.MID; mid != "" {
		first, err := db.MarkEventSeen(ctx, h.Pool, "facebook", "postback:"+mid)
		if err != nil {
			log.Error("dedupe check failed", "step", "dedupe", "err", err)
			return true
		}
		if !first {
			log.Info("duplicate postback skipped")
			return false
		}
		release = func() {
			if err := db.ReleaseEvent(ctx, h.Pool, "facebook", "postback:"+mid); err != nil {
				log.Error("release dedupe claim failed", "err", err)
			}
		}
	}

	o, err := h.lookupPostbackOrder(ctx, orderID, pageFBID)
	if errors.Is(err, pgx.ErrNoRows) {
		log.Warn("postback for unknown order; ignored")
		return false
	}
	if err != nil {
		log.Error("order lookup failed", "step", "lookup_order", "err", err)
		release()
		return true
	}
	if o.psid != nil && *o.psid != ev.Sender.ID {
		log.Warn("postback from a different customer than the order's; ignored", "order_psid", *o.psid)
		return false
	}

	client := h.NewMessenger(o.accessToken)
	// send reports retry for a failed reply to the customer.
	send := func(step string, err error) bool {
		if err == nil {
			return false
		}
		h.recordOrderError(ctx, log, orderID, step, err)
		if messenger.IsPermanent(err) {
			return false
		}
		release()
		return true
	}

	if o.status != "pending_form" && o.status != "pending_payment" {
		log.Info("postback for order that is no longer awaiting payment", "status", o.status)
		return send("reply_status", client.SendText(ev.Sender.ID,
			fmt.Sprintf("Order #%d is already %s. Thank you!", orderID, o.status)))
	}
	if o.amountCents == nil {
		h.recordOrderError(ctx, log, orderID, "choose_payment", errors.New("order has no amount; link a product to the keyword"))
		return send("reply_no_amount", client.SendText(ev.Sender.ID,
			fmt.Sprintf("Sorry, order #%d has no price yet. We'll contact you shortly.", orderID)))
	}
	total := formatMoney(*o.amountCents, o.currency)

	return h.pay(ctx, log, client, ev.Sender.ID, orderID, method, o, total, send, release)
}

// pay sends the order's payment link: the live one if there is one,
// otherwise a new one for method (see payments.Service).
func (h *Handler) pay(ctx context.Context, log *slog.Logger, client *messenger.Client, psid string, orderID int64,
	method string, o postbackOrder, total string, send func(string, error) bool, release func()) (retry bool) {
	label := methodLabel(method)
	unavailable := func() bool {
		h.recordOrderError(ctx, log, orderID, "create_payment", fmt.Errorf("%s is not configured", label))
		return send("reply_payment_unavailable", client.SendText(psid,
			fmt.Sprintf("Sorry, %s payment is unavailable right now. We'll contact you shortly.", label)))
	}
	if h.Payments == nil {
		return unavailable()
	}

	desc := fmt.Sprintf("Order #%d", orderID)
	if o.productName != nil {
		desc += " " + *o.productName
	}
	var customer string
	if o.customer != nil {
		customer = *o.customer
	}
	res, err := h.Payments.Pay(ctx, method, payments.PayRequest{
		OrderID: orderID, AmountCents: *o.amountCents, Currency: o.currency, Description: desc, CustomerName: customer,
	})
	switch {
	case errors.Is(err, payments.ErrMethodUnavailable):
		return unavailable()
	case errors.Is(err, payments.ErrPaymentInProgress):
		// A concurrent tap is creating the page and will send the link.
		log.Info("payment page is being created by another tap")
		return false
	case errors.Is(err, payments.ErrAlreadyPaid):
		return send("reply_already_paid", client.SendText(psid, openPaymentText(orderID, res, err)))
	case err != nil:
		h.recordOrderError(ctx, log, orderID, "create_payment", err)
		release()
		return true
	}
	log = log.With("outcome", res.Outcome, "attempt_method", res.Method)

	switch {
	case res.Outcome == payments.PayNewLink || (res.Outcome == payments.PaySameLink && res.Method == method):
		log.Info("payment link sent")
		lead := label + " payment"
		if res.Outcome == payments.PaySameLink {
			lead = "Here's your " + label + " payment link again"
		}
		return send("send_pay_link", client.SendButtons(psid,
			fmt.Sprintf("%s for order #%d\nTotal: %s\nTap below to pay. The link expires in %s.",
				lead, orderID, total, minutesLeft(h.now(), res.ExpiresAt)),
			[]messenger.Button{{Type: "web_url", Title: "Pay with " + label, URL: res.URL}}))
	default:
		log.Info("payment refused: the order has an open payment")
		return send("reply_payment_open", client.SendText(psid, openPaymentText(orderID, res, nil)))
	}
}

// openPaymentText explains why the order can't get a new payment link.
func openPaymentText(orderID int64, res payments.PayResult, err error) string {
	switch {
	case errors.Is(err, payments.ErrAlreadyPaid) || res.Outcome == payments.PayAlreadyPaid:
		return fmt.Sprintf("Payment received ✅ Order #%d is already paid. Thank you!", orderID)
	case res.Outcome == payments.PaySameLink:
		return fmt.Sprintf("You already have a %s payment link for order #%d. Please pay with that one, or try again after it expires in %s.",
			methodLabel(res.Method), orderID, minutesLeft(time.Now(), res.ExpiresAt))
	default: // PayChecking
		return fmt.Sprintf("We're still confirming your earlier payment for order #%d. Please try again in a few minutes.", orderID)
	}
}

func methodLabel(method string) string {
	switch method {
	case method2C2P:
		return "2C2P"
	case methodPayNow:
		return "PayNow"
	}
	return method
}

// minutesLeft renders the time until t as "12 minutes" (at least 1).
func minutesLeft(now, t time.Time) string {
	m := int(t.Sub(now).Round(time.Minute) / time.Minute)
	if m <= 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", m)
}

// lookupPostbackOrder loads the order, checking it belongs to the Page the
// postback came in on. It returns pgx.ErrNoRows otherwise.
func (h *Handler) lookupPostbackOrder(ctx context.Context, orderID int64, pageFBID string) (o postbackOrder, err error) {
	err = h.Pool.QueryRow(ctx,
		`SELECT o.status, o.psid, o.amount_cents, o.currency, pr.name, o.commenter_name, p.page_access_token
		 FROM orders o
		 JOIN pages p ON p.id = o.page_id
		 LEFT JOIN products pr ON pr.id = o.product_id
		 WHERE o.id = $1 AND p.fb_page_id = $2`,
		orderID, pageFBID,
	).Scan(&o.status, &o.psid, &o.amountCents, &o.currency, &o.productName, &o.customer, &o.accessToken)
	return
}
