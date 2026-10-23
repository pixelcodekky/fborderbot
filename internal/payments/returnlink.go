package payments

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
)

// What the customer did on a payment page, as carried by a return link.
const (
	ReturnDone   = "done"   // finished the page; the result is asked of the provider
	ReturnCancel = "cancel" // left without paying; the page is closed (see Service.Cancel)
)

// ReturnLink is base (the common payment return page) with the invoice, the
// customer's action and a signature over both, e.g.
// https://bot/payments/return?invoice=FB1T2R3&result=cancel&sig=....
// The signature keeps a made-up link from cancelling someone's payment.
func ReturnLink(base string, key []byte, invoiceNo, result string) string {
	q := url.Values{"invoice": {invoiceNo}, "result": {result}, "sig": {returnSig(key, invoiceNo, result)}}
	sep := "?"
	if u, err := url.Parse(base); err == nil && u.RawQuery != "" {
		sep = "&"
	}
	return base + sep + q.Encode()
}

// VerifyReturn reports whether sig is ReturnLink's signature for invoiceNo
// and result.
func VerifyReturn(key []byte, invoiceNo, result, sig string) bool {
	return len(key) > 0 && hmac.Equal([]byte(sig), []byte(returnSig(key, invoiceNo, result)))
}

func returnSig(key []byte, invoiceNo, result string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("payment-return\x00" + invoiceNo + "\x00" + result))
	return hex.EncodeToString(m.Sum(nil))
}
