package payments

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stripe/stripe-go/v78"
	"github.com/stripe/stripe-go/v78/checkout/session"
)

// CreateCheckoutSession creates a Stripe Checkout Session for an order and
// stores the session ID so the webhook can match the payment back to the order.
func CreateCheckoutSession(ctx context.Context, pool *pgxpool.Pool, orderID int64, amountCents int64, currency, productName, successURL, cancelURL string) (checkoutURL string, err error) {
	params := &stripe.CheckoutSessionParams{
		Mode: stripe.String(string(stripe.CheckoutSessionModePayment)),
		PaymentMethodTypes: []*string{
			stripe.String("card"),
			stripe.String("paynow"),
		},
		LineItems: []*stripe.CheckoutSessionLineItemParams{
			{
				Quantity: stripe.Int64(1),
				PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
					Currency:   stripe.String(currency),
					UnitAmount: stripe.Int64(amountCents),
					ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
						Name: stripe.String(productName),
					},
				},
			},
		},
		SuccessURL: stripe.String(successURL),
		CancelURL:  stripe.String(cancelURL),
		Metadata: map[string]string{
			"order_id": itoa(orderID),
		},
	}

	sess, err := session.New(params)
	if err != nil {
		return "", err
	}

	_, err = pool.Exec(ctx,
		`UPDATE orders
		 SET stripe_checkout_session_id = $1, status = 'pending_payment', updated_at = now()
		 WHERE id = $2`,
		sess.ID, orderID,
	)
	if err != nil {
		return "", err
	}

	return sess.URL, nil
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
