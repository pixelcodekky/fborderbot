package fbwebhook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"fb-order-bot/internal/db"
	"fb-order-bot/internal/messenger"
	"fb-order-bot/internal/obs"
	"fb-order-bot/internal/payments"
)

// postbackOrder is what a tap needs to know about its order.
type postbackOrder struct {
	status      string
	psid        *string
	quantity    int
	amountCents *int64
	currency    string
	productName *string
	customer    *string // commenter's name
	accessToken string
}

// processPostback handles a tap on one of the order's buttons: quantity
// ("QTY:<order id>:<n|more>") or payment ("PAY:<order id>:<method>").
// It reports retry like processComment, releasing its dedupe claim first.
func (h *Handler) processPostback(ctx context.Context, pageFBID string, ev messagingEvent) (retry bool) {
	return h.processAction(ctx, pageFBID, ev.Sender.ID, "postback:"+ev.Postback.MID, ev.Postback.Payload)
}

// processMessage handles a message from the customer: a tap on a quantity
// quick reply, or a typed quantity for their latest open order. Anything
// else is left for the Page's staff.
func (h *Handler) processMessage(ctx context.Context, pageFBID string, ev messagingEvent) (retry bool) {
	m := ev.Message
	if m.IsEcho {
		return false
	}
	if m.QuickReply != nil {
		return h.processAction(ctx, pageFBID, ev.Sender.ID, "message:"+m.MID, m.QuickReply.Payload)
	}
	qty, ok := parseQuantity(m.Text)
	if !ok {
		return false
	}
	log := obs.Logger(ctx).With("source", "facebook", "page_fb_id", pageFBID, "psid", ev.Sender.ID)
	var orderID int64
	err := h.Pool.QueryRow(ctx,
		`SELECT o.id FROM orders o JOIN pages p ON p.id = o.page_id
		 WHERE p.fb_page_id = $1 AND o.psid = $2
		   AND o.status IN ('pending_form', 'pending_payment') AND o.unit_price_cents IS NOT NULL
		 ORDER BY o.id DESC LIMIT 1`,
		pageFBID, ev.Sender.ID,
	).Scan(&orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		log.Info("number from a customer without an open order; ignored")
		return false
	}
	if err != nil {
		log.Error("open order lookup failed", "step", "lookup_order", "err", err)
		return true
	}
	return h.processAction(ctx, pageFBID, ev.Sender.ID, "message:"+m.MID, qtyPayload(orderID, qty))
}

// processAction runs one order action for the customer psid. dedupeID
// identifies the tap or message, so a redelivery isn't acted on twice.
func (h *Handler) processAction(ctx context.Context, pageFBID, psid, dedupeID, payload string) (retry bool) {
	log := obs.Logger(ctx).With(
		"source", "facebook",
		"page_fb_id", pageFBID,
		"psid", psid,
		"payload", payload,
	)

	orderID, method, isPay := parsePayPayload(payload)
	qtyOrderID, qty, isQty := parseQtyPayload(payload)
	switch {
	case isPay:
		log = log.With("order_id", orderID, "payment_method", method)
	case isQty:
		orderID = qtyOrderID
		log = log.With("order_id", orderID, "quantity", qty)
	default:
		log.Info("unknown payload; ignored")
		return false
	}

	// Dedupe on the message id: a redelivery has the same mid, a second tap
	// gets a new one.
	release := func() {}
	if dedupeID != "postback:" && dedupeID != "message:" {
		first, err := db.MarkEventSeen(ctx, h.Pool, "facebook", dedupeID)
		if err != nil {
			log.Error("dedupe check failed", "step", "dedupe", "err", err)
			return true
		}
		if !first {
			log.Info("duplicate tap skipped")
			return false
		}
		release = func() {
			if err := db.ReleaseEvent(ctx, h.Pool, "facebook", dedupeID); err != nil {
				log.Error("release dedupe claim failed", "err", err)
			}
		}
	}

	o, err := h.lookupPostbackOrder(ctx, orderID, pageFBID)
	if errors.Is(err, pgx.ErrNoRows) {
		log.Warn("tap for unknown order; ignored")
		return false
	}
	if err != nil {
		log.Error("order lookup failed", "step", "lookup_order", "err", err)
		release()
		return true
	}
	if o.psid != nil && *o.psid != psid {
		log.Warn("tap from a different customer than the order's; ignored", "order_psid", *o.psid)
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
		log.Info("tap for order that is no longer awaiting payment", "status", o.status)
		return send("reply_status", client.SendText(psid,
			fmt.Sprintf("Order #%d is already %s. Thank you!", orderID, o.status)))
	}
	if o.amountCents == nil {
		h.recordOrderError(ctx, log, orderID, "choose_payment", errors.New("order has no amount; link a product to the keyword"))
		return send("reply_no_amount", client.SendText(psid,
			fmt.Sprintf("Sorry, order #%d has no price yet. We'll contact you shortly.", orderID)))
	}

	if isQty {
		return h.setQuantity(ctx, log, client, psid, orderID, qty, o, send, release)
	}
	return h.pay(ctx, log, client, psid, orderID, method, o, formatMoney(*o.amountCents, o.currency), send, release)
}

// setQuantity asks for a quantity (qty 0) or sets it and sends the payment
// buttons with the new total.
func (h *Handler) setQuantity(ctx context.Context, log *slog.Logger, client *messenger.Client, psid string, orderID int64,
	qty int, o postbackOrder, send func(string, error) bool, release func()) (retry bool) {
	if qty == 0 {
		return send("ask_quantity", client.SendQuickReplies(psid,
			fmt.Sprintf("How many would you like? Tap a number below, or type one (1-%d).", payments.MaxQuantity),
			quantityChoices(orderID)))
	}
	if qty < 1 || qty > payments.MaxQuantity {
		return send("reply_quantity_range", client.SendText(psid,
			fmt.Sprintf("Please choose 1 to %d. For a bigger order, message us and we'll help.", payments.MaxQuantity)))
	}
	if h.Payments == nil {
		h.recordOrderError(ctx, log, orderID, "set_quantity", errors.New("payments are not configured"))
		return send("reply_payment_unavailable", client.SendText(psid,
			"Sorry, ordering is unavailable right now. We'll contact you shortly."))
	}

	amount, res, err := h.Payments.SetQuantity(ctx, orderID, qty)
	switch {
	case errors.Is(err, payments.ErrPaymentOpen), errors.Is(err, payments.ErrAlreadyPaid):
		log.Info("quantity change refused: the order has a payment", "outcome", res.Outcome)
		return send("reply_payment_open", client.SendText(psid, quantityLockedText(orderID, res, err)))
	case errors.Is(err, payments.ErrPaymentInProgress):
		log.Info("quantity change refused: a payment link is being made")
		return send("reply_payment_open", client.SendText(psid,
			fmt.Sprintf("Your payment link for order #%d is on its way, so its quantity can't change now.", orderID)))
	case errors.Is(err, payments.ErrNotAwaitingPayment):
		return false // changed under us; the next tap gets "already <status>"
	case errors.Is(err, payments.ErrNoPrice):
		h.recordOrderError(ctx, log, orderID, "set_quantity", err)
		return send("reply_no_amount", client.SendText(psid,
			fmt.Sprintf("Sorry, order #%d has no price yet. We'll contact you shortly.", orderID)))
	case err != nil:
		h.recordOrderError(ctx, log, orderID, "set_quantity", err)
		release()
		return true
	}
	log.Info("quantity set", "amount_cents", amount)
	return send("send_payment_choice", client.SendButtons(psid,
		orderReplyText(o.name(orderID), qty, amount, o.currency), paymentButtons(orderID)))
}

// quantityLockedText explains why the quantity can't change now.
func quantityLockedText(orderID int64, res payments.PayResult, err error) string {
	if errors.Is(err, payments.ErrAlreadyPaid) || res.Outcome != payments.PaySameLink {
		return openPaymentText(orderID, res, err)
	}
	return fmt.Sprintf("You already have a %s payment link for order #%d, so its quantity can't change now. Please pay with that one, or change the quantity after it expires in %s.",
		methodLabel(res.Method), orderID, minutesLeft(time.Now(), res.ExpiresAt))
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
	if o.quantity > 1 {
		desc += " x " + strconv.Itoa(o.quantity)
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
	case errors.Is(err, payments.ErrOrderChanged):
		// The quantity changed between reading the order and paying.
		return send("reply_order_changed", client.SendText(psid,
			fmt.Sprintf("Order #%d was just updated. Please choose a payment method again.", orderID)))
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
	case methodStripe:
		return "Stripe"
	case methodPayNow: // an attempt made before Stripe took both
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

// name is what the order is for, e.g. "test sales"; "order #42" without a
// product.
func (o postbackOrder) name(orderID int64) string {
	if o.productName != nil {
		return *o.productName
	}
	return fmt.Sprintf("order #%d", orderID)
}

// lookupPostbackOrder loads the order, checking it belongs to the Page the
// tap came in on. It returns pgx.ErrNoRows otherwise.
func (h *Handler) lookupPostbackOrder(ctx context.Context, orderID int64, pageFBID string) (o postbackOrder, err error) {
	err = h.Pool.QueryRow(ctx,
		`SELECT o.status, o.psid, o.quantity, o.amount_cents, o.currency, pr.name, o.commenter_name, p.page_access_token
		 FROM orders o
		 JOIN pages p ON p.id = o.page_id
		 LEFT JOIN products pr ON pr.id = o.product_id
		 WHERE o.id = $1 AND p.fb_page_id = $2`,
		orderID, pageFBID,
	).Scan(&o.status, &o.psid, &o.quantity, &o.amountCents, &o.currency, &o.productName, &o.customer, &o.accessToken)
	return
}
