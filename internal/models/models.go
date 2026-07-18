package models

import "time"

type Order struct {
	ID                       int64
	PageID                   int64
	ProductID                *int64
	FBCommentID              string
	PSID                     *string
	Quantity                 int
	AmountCents              *int64
	Currency                 string
	CustomerEmail            *string
	PaymentMethod            *string
	Status                   string
	StripeCheckoutSessionID  *string
	StripePaymentIntentID    *string
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

const (
	StatusPendingForm    = "pending_form"
	StatusPendingPayment = "pending_payment"
	StatusPaid           = "paid"
	StatusPaymentFailed  = "payment_failed"
	StatusCancelled      = "cancelled"
)

type Product struct {
	ID          int64
	PageID      int64
	Name        string
	Description string
	PriceCents  int64
	Currency    string
	ImageURL    string
	Active      bool
}
