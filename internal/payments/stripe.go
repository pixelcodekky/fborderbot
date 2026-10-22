package payments

import (
	"context"
	"strconv"
	"time"

	"github.com/stripe/stripe-go/v78"
)

// CheckoutSessions is the part of Stripe's Checkout Sessions API used here;
// session.Client implements it.
type CheckoutSessions interface {
	New(params *stripe.CheckoutSessionParams) (*stripe.CheckoutSession, error)
	Get(id string, params *stripe.CheckoutSessionParams) (*stripe.CheckoutSession, error)
	Expire(id string, params *stripe.CheckoutSessionExpireParams) (*stripe.CheckoutSession, error)
}

// Stripe is Stripe Checkout as a Provider. A Checkout Session can be expired
// on demand, so an expired attempt is closed for certain before a new page
// is made.
type Stripe struct {
	Sessions   CheckoutSessions
	Methods    []string // payment method types, e.g. "paynow"; empty means card and PayNow
	SuccessURL string
	CancelURL  string
	PageExpiry time.Duration // Stripe allows 30 minutes to 24 hours; defaults to 30 minutes
}

func (p *Stripe) Expiry() time.Duration {
	if p.PageExpiry < 30*time.Minute {
		return 30 * time.Minute
	}
	return p.PageExpiry
}

func (p *Stripe) CreatePage(ctx context.Context, pg Page) (url, ref string, err error) {
	methods := p.Methods
	if len(methods) == 0 {
		methods = []string{"card", "paynow"}
	}
	params := &stripe.CheckoutSessionParams{
		Mode:               stripe.String(string(stripe.CheckoutSessionModePayment)),
		PaymentMethodTypes: stripe.StringSlice(methods),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Quantity: stripe.Int64(1),
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency:   stripe.String(pg.Currency),
					UnitAmount: stripe.Int64(pg.AmountCents),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name: stripe.String(pg.Description),
					},
				},
			},
		},
		SuccessURL: stripe.String(p.SuccessURL),
		CancelURL:  stripe.String(p.CancelURL),
		// The webhook finds the attempt by this.
		ClientReferenceID: stripe.String(pg.InvoiceNo),
		// A minute past ours: Stripe needs at least 30 minutes from its own
		// clock, and Check expires the session once ours has passed.
		ExpiresAt: stripe.Int64(pg.ExpiresAt.Add(time.Minute).Unix()),
		Metadata: map[string]string{
			"order_id":   strconv.FormatInt(pg.OrderID, 10),
			"invoice_no": pg.InvoiceNo,
		},
	}
	params.Context = ctx
	// A retried request returns the same session instead of a second one.
	params.SetIdempotencyKey("checkout-" + pg.InvoiceNo)

	sess, err := p.Sessions.New(params)
	if err != nil {
		return "", "", err
	}
	return sess.URL, sess.ID, nil
}

func (p *Stripe) Check(ctx context.Context, a Attempt) (Result, error) {
	params := &stripe.CheckoutSessionParams{}
	params.Context = ctx
	sess, err := p.Sessions.Get(a.Ref, params)
	if err != nil {
		return Result{}, err
	}
	if sess.Status == stripe.CheckoutSessionStatusOpen && a.Expired {
		// Close it so it can't be paid alongside a new page. If it completes
		// first, Expire fails and the next check sees the payment.
		exp := &stripe.CheckoutSessionExpireParams{}
		exp.Context = ctx
		if sess, err = p.Sessions.Expire(a.Ref, exp); err != nil {
			return Result{}, err
		}
	}
	r := StripeResult(sess)
	r.InvoiceNo = a.InvoiceNo
	return r, nil
}

// StripeResult converts a Checkout Session. A completed session whose
// payment is still processing (an async method) is ResultPending.
func StripeResult(sess *stripe.CheckoutSession) Result {
	r := Result{
		InvoiceNo:   sess.ClientReferenceID,
		AmountCents: sess.AmountTotal,
		Currency:    string(sess.Currency),
		RespCode:    string(sess.PaymentStatus),
	}
	if sess.PaymentIntent != nil {
		r.TranRef = sess.PaymentIntent.ID
	}
	if len(sess.PaymentMethodTypes) > 0 {
		r.ChannelCode = sess.PaymentMethodTypes[0]
	}
	switch {
	case sess.Status == stripe.CheckoutSessionStatusComplete &&
		(sess.PaymentStatus == stripe.CheckoutSessionPaymentStatusPaid || sess.PaymentStatus == stripe.CheckoutSessionPaymentStatusNoPaymentRequired):
		r.Status = ResultPaid
	case sess.Status == stripe.CheckoutSessionStatusExpired:
		r.Status = ResultFailed
		r.RespDesc = "checkout session expired"
	default:
		r.Status = ResultPending
	}
	return r
}
