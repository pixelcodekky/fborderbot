package payments

import (
	"context"
	"errors"
	"time"

	"fb-order-bot/internal/twoc2p"
)

// TwoC2P is the 2C2P hosted payment page as a Provider. 2C2P rejects
// payments after the page's paymentExpiry, so an expired page needs only a
// Payment Inquiry to settle.
type TwoC2P struct {
	Client     *twoc2p.Client
	PageExpiry time.Duration // defaults to 20 minutes
}

func (p *TwoC2P) Expiry() time.Duration {
	if p.PageExpiry <= 0 {
		return 20 * time.Minute
	}
	return p.PageExpiry
}

func (p *TwoC2P) CreatePage(ctx context.Context, pg Page) (url, ref string, err error) {
	url, err = p.Client.CreatePaymentToken(ctx, twoc2p.PaymentTokenRequest{
		InvoiceNo:    pg.InvoiceNo,
		Description:  pg.Description,
		AmountCents:  pg.AmountCents,
		Currency:     pg.Currency,
		CustomerName: pg.CustomerName,
		ExpiresAt:    pg.ExpiresAt,
	})
	return url, "", err
}

func (p *TwoC2P) Check(ctx context.Context, a Attempt) (Result, error) {
	r, err := p.Client.Inquire(ctx, a.InvoiceNo)
	var re *twoc2p.RespError
	if errors.As(err, &re) {
		// 2C2P has no payment for the invoice (e.g. "not found").
		return Result{InvoiceNo: a.InvoiceNo, Status: ResultFailed, RespCode: re.RespCode, RespDesc: re.RespDesc}, nil
	}
	if err != nil {
		return Result{}, err
	}
	return TwoC2PResult(r), nil
}

// TwoC2PResult converts a 2C2P result (notification or inquiry). The amount
// is the one requested, in the order's currency, even when the merchant
// settles in another.
func TwoC2PResult(r *twoc2p.Result) Result {
	out := Result{
		InvoiceNo:   r.InvoiceNo,
		TranRef:     r.TranRef,
		ChannelCode: r.ChannelCode,
		RespCode:    r.RespCode,
		RespDesc:    r.RespDesc,
	}
	switch {
	case r.Paid():
		out.Status = ResultPaid
		amount, currency := r.RequestedAmount()
		out.Currency = currency
		if cents, ok := ParseCents(amount); ok {
			out.AmountCents = cents
		} else {
			out.AmountCents = -1
		}
	case r.RespCode == twoc2p.RespInProgress:
		out.Status = ResultPending
	default:
		out.Status = ResultFailed
	}
	return out
}
