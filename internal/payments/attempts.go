package payments

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"fb-order-bot/internal/db"
	"fb-order-bot/internal/obs"
)

// Payment methods: the customer's choice, and payment_attempts.provider.
const (
	Method2C2P   = "2c2p"
	MethodStripe = "stripe" // Stripe Checkout: card and PayNow
	// MethodPayNow is the earlier PayNow-only Stripe Checkout. It is no
	// longer offered, but its attempts are still settled and shown.
	MethodPayNow = "paynow"
)

// Service takes payments for orders without letting one order be paid twice,
// whichever method the customer picks. Each payment page is a
// payment_attempts row, and an order has at most one pending attempt (a
// partial unique index guarantees it), across all methods:
//
//   - While the attempt's page is live, a tap sends the same link again; a
//     tap for another method is told to use that link.
//   - Once the page has expired, its provider settles the attempt (asking
//     for, or closing, the payment) before a new page can be made.
//   - A result is applied only once: attempts move out of pending with a
//     conditional update, and an order is marked paid only if it isn't yet.
type Service struct {
	Pool      *pgxpool.Pool
	Providers map[string]Provider // by payment method; a missing one is unavailable
	Now       func() time.Time    // defaults to time.Now; overridden in tests
}

// Provider is one payment method's hosted payment page.
type Provider interface {
	// Expiry is how long a new page accepts payment.
	Expiry() time.Duration
	// CreatePage makes the payment page for a new attempt. ref is the
	// provider's id for it, kept for Check; it may be empty.
	CreatePage(ctx context.Context, p Page) (url, ref string, err error)
	// Check asks the provider for an attempt's result. For an expired
	// attempt it should make sure the page can no longer take payment, so
	// that ResultFailed is final.
	Check(ctx context.Context, a Attempt) (Result, error)
}

// Page is what a provider needs to make a payment page.
type Page struct {
	PayRequest
	InvoiceNo string // unique per attempt, alphanumeric, max 50
	ExpiresAt time.Time
}

// Attempt identifies an attempt to its provider.
type Attempt struct {
	InvoiceNo string
	Ref       string // as returned by CreatePage
	Expired   bool   // the page is past its expiry
}

type ResultStatus int

const (
	ResultPending ResultStatus = iota // no final answer yet
	ResultPaid
	ResultFailed // not paid; final once the page has expired
)

// Result is a provider's answer about one attempt, from a notification or a
// Check.
type Result struct {
	InvoiceNo   string
	Status      ResultStatus
	AmountCents int64  // ResultPaid: what was paid; -1 when unreadable
	Currency    string // ResultPaid
	TranRef     string // the provider's payment id
	ChannelCode string // e.g. "VI", "paynow"
	RespCode    string
	RespDesc    string
}

// creatingTimeout is how long a pending attempt without a payment URL counts
// as being created by another request. After that it is treated as abandoned:
// its URL never reached the customer, so nobody can pay it.
const creatingTimeout = time.Minute

// ErrPaymentInProgress means another request is creating the order's payment
// page right now; that request sends the link.
var ErrPaymentInProgress = errors.New("another request is creating this order's payment page")

// ErrAlreadyPaid means the order already has a successful payment.
var ErrAlreadyPaid = errors.New("order already has a successful payment")

// ErrMethodUnavailable means the payment method isn't configured.
var ErrMethodUnavailable = errors.New("payment method is not configured")

// ErrOrderChanged means the order's amount changed (e.g. its quantity) since
// it was read; read it again.
var ErrOrderChanged = errors.New("order amount changed")

type PayOutcome int

const (
	PayNewLink     PayOutcome = iota // a new payment page
	PaySameLink                      // the order's live payment page, again (see PayResult.Method)
	PayAlreadyPaid                   // the open attempt turned out to be paid; the order is now paid
	PayChecking                      // the open attempt's result isn't known yet
)

type PayRequest struct {
	OrderID      int64
	AmountCents  int64
	Currency     string
	Description  string
	CustomerName string
}

type PayResult struct {
	Outcome   PayOutcome
	Method    string    // the attempt's payment method; may differ from the one asked for
	URL       string    // PayNewLink, PaySameLink
	ExpiresAt time.Time // PayNewLink, PaySameLink
}

// Pay returns the order's payment page: the live one if there is one (of
// whatever method), otherwise a new one with method. See PayOutcome for the
// other answers.
func (s *Service) Pay(ctx context.Context, method string, req PayRequest) (PayResult, error) {
	if s.Providers[method] == nil {
		return PayResult{}, ErrMethodUnavailable
	}
	res, open, err := s.Current(ctx, req.OrderID)
	if err != nil || open {
		return res, err
	}
	return s.create(ctx, method, req)
}

// Current settles the order's pending attempt, if any. open reports that it
// still stands in the way of a new payment (live page, paid, or unknown).
func (s *Service) Current(ctx context.Context, orderID int64) (res PayResult, open bool, err error) {
	var paid bool
	if err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM payment_attempts WHERE order_id = $1 AND status = 'succeeded')`,
		orderID,
	).Scan(&paid); err != nil {
		return res, false, err
	}
	if paid {
		return res, false, ErrAlreadyPaid
	}

	var a struct {
		id         int64
		method     string
		invoiceNo  string
		ref        *string
		paymentURL *string
		expiresAt  time.Time
		createdAt  time.Time
	}
	err = s.Pool.QueryRow(ctx,
		`SELECT id, provider, invoice_no, provider_ref, payment_url, expires_at, created_at
		 FROM payment_attempts WHERE order_id = $1 AND status = 'pending'`,
		orderID,
	).Scan(&a.id, &a.method, &a.invoiceNo, &a.ref, &a.paymentURL, &a.expiresAt, &a.createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, false, nil
	}
	if err != nil {
		return res, false, err
	}
	log := obs.Logger(ctx).With("order_id", orderID, "invoice_no", a.invoiceNo, "attempt_method", a.method)
	now := s.now()
	res.Method = a.method

	if a.paymentURL == nil {
		if now.Sub(a.createdAt) < creatingTimeout {
			return res, true, ErrPaymentInProgress
		}
		log.Warn("payment attempt abandoned before its link was sent; closing it")
		return res, false, s.markFailed(ctx, a.id, "", "abandoned before the payment link was sent")
	}
	if now.Before(a.expiresAt) {
		res.Outcome, res.URL, res.ExpiresAt = PaySameLink, *a.paymentURL, a.expiresAt
		return res, true, nil
	}

	// The page has expired, but its notification may have been lost: ask
	// the provider before letting a new page be made.
	p := s.Providers[a.method]
	if p == nil {
		log.Error("expired attempt's payment method is no longer configured; can't settle it")
		res.Outcome = PayChecking
		return res, true, nil
	}
	var ref string
	if a.ref != nil {
		ref = *a.ref
	}
	r, err := p.Check(ctx, Attempt{InvoiceNo: a.invoiceNo, Ref: ref, Expired: true})
	switch {
	case err != nil:
		log.Warn("payment check failed; keeping the attempt open", "err", err)
		res.Outcome = PayChecking
		return res, true, nil
	case r.Status == ResultPending:
		res.Outcome = PayChecking
		return res, true, nil
	case r.Status == ResultFailed:
		log.Info("no payment for expired page; closing it", "resp_code", r.RespCode)
		return res, false, s.markFailed(ctx, a.id, r.RespCode, r.RespDesc)
	}
	r.InvoiceNo = a.invoiceNo
	applied, err := s.Apply(ctx, r)
	if err != nil {
		return res, true, err
	}
	switch applied.Outcome {
	case ApplyPaid, ApplyDoublePayment:
		res.Outcome = PayAlreadyPaid
		return res, true, nil
	case ApplyMismatch:
		res.Outcome = PayChecking // needs a person
		return res, true, nil
	}
	return res, false, nil // failed: closed
}

func (s *Service) create(ctx context.Context, method string, req PayRequest) (PayResult, error) {
	p := s.Providers[method]
	now := s.now()
	expiresAt := now.Add(p.Expiry()).Truncate(time.Second)
	invoiceNo := InvoiceNo(req.OrderID, now)

	// The order row is locked while claiming, so a quantity change (see
	// SetQuantity) can't slip in between reading the amount and the claim.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return PayResult{}, err
	}
	defer tx.Rollback(ctx)
	var orderAmount *int64
	var orderCurrency string
	if err := tx.QueryRow(ctx,
		`SELECT amount_cents, currency FROM orders WHERE id = $1 FOR UPDATE`, req.OrderID,
	).Scan(&orderAmount, &orderCurrency); err != nil {
		return PayResult{}, fmt.Errorf("lock order: %w", err)
	}
	if orderAmount == nil || *orderAmount != req.AmountCents || !strings.EqualFold(orderCurrency, req.Currency) {
		return PayResult{}, ErrOrderChanged
	}

	// Claim the order's one pending slot before calling the provider. A
	// concurrent tap loses on the partial unique index. retry_of links the
	// attempt to the order's last failed one, which can be retried only once.
	var attemptID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO payment_attempts (order_id, provider, invoice_no, amount_cents, currency, expires_at, retry_of)
		 VALUES ($1, $2, $3, $4, $5, $6,
		         (SELECT f.id FROM payment_attempts f
		          WHERE f.order_id = $1 AND f.status = 'failed'
		            AND NOT EXISTS (SELECT 1 FROM payment_attempts r WHERE r.retry_of = f.id)
		          ORDER BY f.id DESC LIMIT 1))
		 RETURNING id`,
		req.OrderID, method, invoiceNo, req.AmountCents, req.Currency, expiresAt,
	).Scan(&attemptID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName != "payment_attempts_invoice_no_key" {
		// The pending slot, or the failed attempt's one retry, was taken.
		return PayResult{}, ErrPaymentInProgress
	}
	if err != nil {
		return PayResult{}, fmt.Errorf("create payment attempt: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return PayResult{}, fmt.Errorf("create payment attempt: %w", err)
	}

	url, ref, err := p.CreatePage(ctx, Page{PayRequest: req, InvoiceNo: invoiceNo, ExpiresAt: expiresAt})
	if err != nil {
		// No URL reached the customer, so nobody can pay this attempt.
		if ferr := s.markFailed(ctx, attemptID, "", err.Error()); ferr != nil {
			obs.Logger(ctx).Error("close failed payment attempt", "invoice_no", invoiceNo, "err", ferr)
		}
		return PayResult{}, err
	}

	if _, err := s.Pool.Exec(ctx,
		`UPDATE payment_attempts SET payment_url = $1, provider_ref = $2, updated_at = now() WHERE id = $3`,
		url, nullIfEmpty(ref), attemptID,
	); err != nil {
		// The URL isn't stored, so it is never sent; the attempt ends up
		// abandoned after creatingTimeout.
		return PayResult{}, fmt.Errorf("store payment url: %w", err)
	}
	if _, err := s.Pool.Exec(ctx,
		`UPDATE orders SET status = 'pending_payment', payment_method = $1, updated_at = now()
		 WHERE id = $2 AND status IN ('pending_form', 'pending_payment')`,
		method, req.OrderID,
	); err != nil {
		return PayResult{}, fmt.Errorf("mark order pending_payment: %w", err)
	}
	return PayResult{Outcome: PayNewLink, Method: method, URL: url, ExpiresAt: expiresAt}, nil
}

func (s *Service) markFailed(ctx context.Context, attemptID int64, respCode, respDesc string) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE payment_attempts
		 SET status = 'failed', resp_code = $1, resp_desc = $2, updated_at = now()
		 WHERE id = $3 AND status = 'pending'`,
		nullIfEmpty(respCode), nullIfEmpty(truncate(respDesc, 255)), attemptID,
	)
	return err
}

type ApplyOutcome int

const (
	ApplyNone          ApplyOutcome = iota // nothing was applied (zero value)
	ApplyPaid                              // the order is now paid
	ApplyFailed                            // the attempt is now failed; the customer can pay again
	ApplyAlreadyFinal                      // the attempt already had a result; nothing changed
	ApplyNotFound                          // no attempt with that invoice
	ApplyMismatch                          // paid amount differs from the attempt's; not applied
	ApplyDoublePayment                     // paid, but the order was already paid: refund by hand
)

// Applied is a result's effect, and what's needed to tell the customer.
type Applied struct {
	Outcome         ApplyOutcome
	OrderID         int64
	PSID            *string
	ProductName     string
	PageAccessToken string
	ReceiptToken    string // ApplyPaid: for ReceiptURL
}

// Apply records a final result (ResultPaid or ResultFailed) on its attempt
// and order. It is safe to call repeatedly and concurrently with the same
// result. A ResultPending is ignored.
func (s *Service) Apply(ctx context.Context, r Result) (Applied, error) {
	log := obs.Logger(ctx).With("invoice_no", r.InvoiceNo, "resp_code", r.RespCode)
	var (
		ap          Applied
		attemptID   int64
		method      string
		status      string
		amountCents int64
		currency    string
	)
	if r.Status == ResultPending {
		return ap, nil
	}
	err := s.Pool.QueryRow(ctx,
		`SELECT a.id, a.provider, a.status, a.amount_cents, a.currency,
		        o.id, o.psid, COALESCE(pr.name, 'your order'), p.page_access_token
		 FROM payment_attempts a
		 JOIN orders o ON o.id = a.order_id
		 JOIN pages p ON p.id = o.page_id
		 LEFT JOIN products pr ON pr.id = o.product_id
		 WHERE a.invoice_no = $1`,
		r.InvoiceNo,
	).Scan(&attemptID, &method, &status, &amountCents, &currency, &ap.OrderID, &ap.PSID, &ap.ProductName, &ap.PageAccessToken)
	if errors.Is(err, pgx.ErrNoRows) {
		log.Error("payment result for unknown invoice")
		ap.Outcome = ApplyNotFound
		return ap, nil
	}
	if err != nil {
		return ap, fmt.Errorf("look up payment attempt: %w", err)
	}
	log = log.With("order_id", ap.OrderID, "payment_method", method)

	if r.Status == ResultFailed {
		tag, err := s.Pool.Exec(ctx,
			`UPDATE payment_attempts
			 SET status = 'failed', resp_code = $1, resp_desc = $2, updated_at = now()
			 WHERE id = $3 AND status = 'pending'`,
			nullIfEmpty(r.RespCode), nullIfEmpty(truncate(r.RespDesc, 255)), attemptID,
		)
		if err != nil {
			return ap, fmt.Errorf("mark attempt failed: %w", err)
		}
		if tag.RowsAffected() == 0 {
			ap.Outcome = ApplyAlreadyFinal
			return ap, nil
		}
		log.Info("payment not completed", "resp_desc", r.RespDesc)
		ap.Outcome = ApplyFailed
		return ap, nil
	}

	if r.AmountCents != amountCents || !strings.EqualFold(r.Currency, currency) {
		s.recordOrderError(ctx, log, ap.OrderID, "amount_mismatch",
			fmt.Errorf("invoice %s paid %s, expected %s", r.InvoiceNo, formatCents(r.AmountCents, r.Currency), formatCents(amountCents, currency)))
		ap.Outcome = ApplyMismatch
		return ap, nil
	}

	// The attempt and the order change together, so a retry after a failure
	// in between can't find the attempt done and the order not.
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return ap, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE payment_attempts
		 SET status = 'succeeded', resp_code = $1, resp_desc = $2, tran_ref = $3, channel_code = $4, updated_at = now()
		 WHERE id = $5 AND status <> 'succeeded'`,
		nullIfEmpty(r.RespCode), nullIfEmpty(truncate(r.RespDesc, 255)), nullIfEmpty(r.TranRef), nullIfEmpty(r.ChannelCode), attemptID,
	)
	if err != nil {
		return ap, fmt.Errorf("mark attempt succeeded: %w", err)
	}
	if tag.RowsAffected() == 0 {
		ap.Outcome = ApplyAlreadyFinal
		return ap, nil
	}
	if status == "failed" {
		// Money arrived for an attempt we had closed. Accept it; if the order
		// was paid another way meanwhile, that's caught just below.
		log.Error("payment arrived for an attempt marked failed", "step", "late_payment")
	}

	// The receipt token makes the receipt link (see Receipt): random and
	// unguessable, since the link is the only key to the receipt.
	err = tx.QueryRow(ctx,
		`UPDATE orders
		 SET status = 'paid', payment_method = $1,
		     receipt_token = COALESCE(receipt_token, replace(gen_random_uuid()::text, '-', '')),
		     last_error = NULL, last_error_step = NULL, last_error_at = NULL, updated_at = now()
		 WHERE id = $2 AND status <> 'paid'
		 RETURNING receipt_token`,
		method, ap.OrderID,
	).Scan(&ap.ReceiptToken)
	alreadyPaid := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !alreadyPaid {
		return ap, fmt.Errorf("mark order paid: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ap, err
	}
	if alreadyPaid {
		s.recordOrderError(ctx, log, ap.OrderID, "double_payment",
			fmt.Errorf("%s invoice %s (ref %s) paid an order that was already paid; refund it", method, r.InvoiceNo, r.TranRef))
		ap.Outcome = ApplyDoublePayment
		return ap, nil
	}
	log.Info("order paid", "tran_ref", r.TranRef, "channel_code", r.ChannelCode)
	ap.Outcome = ApplyPaid
	return ap, nil
}

type SettleStatus int

const (
	SettlePaid           SettleStatus = iota // the order is paid (now or before)
	SettleNotPaid                            // the provider has no successful payment for the invoice
	SettlePending                            // no final answer yet; the notification or a later tap settles it
	SettleUnknownInvoice                     // not one of our invoices
	SettleCancelled                          // Cancel closed the page unpaid
)

// Settled is what Settle found out.
type Settled struct {
	Status       SettleStatus
	OrderID      int64
	PageFBID     string // for a link back to the Page in Messenger
	AmountCents  int64
	Currency     string
	ReceiptToken string  // SettlePaid: for ReceiptURL
	Applied      Applied // set when this call recorded a payment (Applied.Outcome == ApplyPaid)
}

// Canceler is a Provider that can close a live page on demand.
type Canceler interface {
	// Cancel closes the attempt's page and reports its result, as Check does
	// for an expired attempt.
	Cancel(ctx context.Context, a Attempt) (Result, error)
}

// Settle checks an invoice's result when the customer's browser comes back
// from the payment page, so the order doesn't depend on the provider's
// notification arriving. Only a success is recorded: until the page expires
// the customer may still pay on it, so a failure leaves the attempt pending
// (the notification or a tap after expiry closes it).
func (s *Service) Settle(ctx context.Context, invoiceNo string) (Settled, error) {
	return s.settle(ctx, invoiceNo, false)
}

// Cancel is Settle for a customer who left the payment page without paying.
// If the provider is a Canceler the page is closed and the attempt failed,
// so the customer can pick a method again straight away (SettleCancelled);
// otherwise it is just Settle. The caller must have checked that the
// customer's return link is genuine (VerifyReturn).
func (s *Service) Cancel(ctx context.Context, invoiceNo string) (Settled, error) {
	return s.settle(ctx, invoiceNo, true)
}

func (s *Service) settle(ctx context.Context, invoiceNo string, cancel bool) (Settled, error) {
	var st Settled
	var status, method string
	var ref *string
	err := s.Pool.QueryRow(ctx,
		`SELECT a.status, a.provider, a.provider_ref, a.amount_cents, a.currency, o.id, p.fb_page_id, COALESCE(o.receipt_token, '')
		 FROM payment_attempts a
		 JOIN orders o ON o.id = a.order_id
		 JOIN pages p ON p.id = o.page_id
		 WHERE a.invoice_no = $1`,
		invoiceNo,
	).Scan(&status, &method, &ref, &st.AmountCents, &st.Currency, &st.OrderID, &st.PageFBID, &st.ReceiptToken)
	if errors.Is(err, pgx.ErrNoRows) {
		st.Status = SettleUnknownInvoice
		return st, nil
	}
	if err != nil {
		return st, fmt.Errorf("look up payment attempt: %w", err)
	}
	switch status {
	case "succeeded":
		st.Status = SettlePaid
		return st, nil
	case "pending":
	default: // already closed unpaid, e.g. a second visit to the cancel link
		st.Status = SettleNotPaid
		if cancel {
			st.Status = SettleCancelled
		}
		return st, nil
	}

	log := obs.Logger(ctx).With("invoice_no", invoiceNo, "order_id", st.OrderID)
	p := s.Providers[method]
	if p == nil {
		st.Status = SettlePending
		return st, nil
	}
	a := Attempt{InvoiceNo: invoiceNo}
	if ref != nil {
		a.Ref = *ref
	}
	c, canCancel := p.(Canceler)
	cancel = cancel && canCancel
	var r Result
	if cancel {
		r, err = c.Cancel(ctx, a)
	} else {
		r, err = p.Check(ctx, a)
	}
	switch {
	case err != nil:
		log.Warn("payment check on return failed", "cancel", cancel, "err", err)
		st.Status = SettlePending
		return st, nil
	case r.Status == ResultPending:
		st.Status = SettlePending
		return st, nil
	case r.Status == ResultFailed && !cancel:
		log.Info("payment check on return: not paid", "resp_code", r.RespCode, "resp_desc", r.RespDesc)
		st.Status = SettleNotPaid
		return st, nil
	}

	r.InvoiceNo = invoiceNo
	if r.Status == ResultFailed {
		r.RespDesc = "cancelled by the customer"
	}
	st.Applied, err = s.Apply(ctx, r)
	if err != nil {
		return st, err
	}
	switch st.Applied.Outcome {
	case ApplyPaid:
		st.Status, st.ReceiptToken = SettlePaid, st.Applied.ReceiptToken
	case ApplyAlreadyFinal, ApplyDoublePayment:
		if r.Status == ResultFailed {
			// Closed meanwhile, perhaps paid: read back what it ended as.
			return s.settle(ctx, invoiceNo, cancel)
		}
		st.Status = SettlePaid
	case ApplyFailed:
		log.Info("payment cancelled by the customer")
		st.Status = SettleCancelled
	default: // a mismatch needs a person
		st.Status = SettlePending
	}
	return st, nil
}

func (s *Service) recordOrderError(ctx context.Context, log *slog.Logger, orderID int64, step string, err error) {
	log.Error(step+" failed", "step", step, "err", err)
	if dbErr := db.RecordOrderError(ctx, s.Pool, orderID, step, err); dbErr != nil {
		log.Error("record order error failed", "err", dbErr)
	}
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// InvoiceNo is our reference for one payment attempt on an order, e.g.
// "FB42T1792459910123R9F3A": alphanumeric, max 50, unique per attempt.
func InvoiceNo(orderID int64, now time.Time) string {
	var r [2]byte
	rand.Read(r[:])
	return fmt.Sprintf("FB%dT%dR%X", orderID, now.UnixMilli(), r)
}

// ParseCents reads a decimal amount ("100.00", "100.5") as cents, without
// floating point. ok is false for anything else, including sub-cent amounts.
func ParseCents(amount string) (cents int64, ok bool) {
	whole, frac, _ := strings.Cut(amount, ".")
	if len(frac) > 2 {
		if strings.Trim(frac[2:], "0") != "" {
			return 0, false
		}
		frac = frac[:2]
	}
	frac += strings.Repeat("0", 2-len(frac))
	if whole == "" || strings.HasPrefix(whole, "-") || strings.HasPrefix(whole, "+") {
		return 0, false
	}
	cents, err := strconv.ParseInt(whole+frac, 10, 64)
	return cents, err == nil
}

func formatCents(cents int64, currency string) string {
	return fmt.Sprintf("%d.%02d %s", cents/100, cents%100, strings.ToUpper(currency))
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
