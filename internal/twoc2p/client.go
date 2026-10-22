// Package twoc2p is a client for 2C2P PGW v4.3: the Payment Token API, which
// returns the hosted payment page URL, the Payment Inquiry API, and the
// backend notification 2C2P posts once the customer has paid. All carry
// {"payload": "<JWT>"} signed HS256 with the merchant secret key.
package twoc2p

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	_ "time/tzdata" // paymentExpiry's time zone, also on images without zoneinfo
)

const SandboxBaseURL = "https://sandbox-pgw.2c2p.com"

// Response codes.
const (
	RespSuccess    = "0000" // token created / payment succeeded
	RespInProgress = "2001" // payment still in progress; inquire again later
)

var defaultLocation = mustLoadLocation("Asia/Singapore")

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

type Client struct {
	BaseURL           string // e.g. SandboxBaseURL
	MerchantID        string
	SecretKey         string
	FrontendReturnURL string // where 2C2P sends the browser after paying
	BackendReturnURL  string // where 2C2P posts the payment notification
	Locale            string // defaults to "en"
	// Location is the time zone paymentExpiry is written in. Defaults to
	// Asia/Singapore.
	Location   *time.Location
	HTTPClient *http.Client
}

func New(baseURL, merchantID, secretKey, frontendReturnURL, backendReturnURL string) *Client {
	return &Client{
		BaseURL:           baseURL,
		MerchantID:        merchantID,
		SecretKey:         secretKey,
		FrontendReturnURL: frontendReturnURL,
		BackendReturnURL:  backendReturnURL,
		// No retries: a timed-out request may still have created the token.
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// PaymentTokenRequest is one hosted-page payment.
type PaymentTokenRequest struct {
	InvoiceNo    string // alphanumeric, max 50; 2C2P never accepts one twice
	Description  string
	AmountCents  int64
	Currency     string // e.g. "sgd"
	CustomerName string // optional, prefills the payment page
	// ExpiresAt is when the payment page stops accepting payment; 2C2P
	// rejects later payments. Zero leaves 2C2P's default (20 minutes).
	ExpiresAt time.Time
}

// RespError is a 2C2P answer with a respCode other than RespSuccess.
type RespError struct {
	RespCode string
	RespDesc string
}

func (e *RespError) Error() string {
	return fmt.Sprintf("2c2p respCode %s: %s", e.RespCode, e.RespDesc)
}

// CreatePaymentToken calls the Payment Token API and returns the URL of the
// 2C2P hosted payment page for the invoice.
func (c *Client) CreatePaymentToken(ctx context.Context, req PaymentTokenRequest) (webPaymentURL string, err error) {
	claims := map[string]any{
		"merchantID":        c.MerchantID,
		"invoiceNo":         req.InvoiceNo,
		"description":       req.Description,
		"amount":            json.Number(formatAmount(req.AmountCents)),
		"currencyCode":      strings.ToUpper(req.Currency),
		"locale":            c.locale(),
		"frontendReturnUrl": c.FrontendReturnURL,
		"backendReturnUrl":  c.BackendReturnURL,
	}
	if !req.ExpiresAt.IsZero() {
		claims["paymentExpiry"] = req.ExpiresAt.In(c.location()).Format("2006-01-02 15:04:05")
	}
	if req.CustomerName != "" {
		claims["uiParams"] = map[string]any{"userInfo": map[string]string{"name": req.CustomerName}}
	}

	resp, _, err := c.postJWT(ctx, "/payment/4.3/paymentToken", claims)
	if err != nil {
		return "", err
	}
	var out struct {
		WebPaymentURL string `json:"webPaymentUrl"`
		RespCode      string `json:"respCode"`
		RespDesc      string `json:"respDesc"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return "", fmt.Errorf("2c2p paymentToken: decode response: %w", err)
	}
	if out.RespCode != RespSuccess {
		return "", &RespError{RespCode: out.RespCode, RespDesc: out.RespDesc}
	}
	if out.WebPaymentURL == "" {
		return "", errors.New("2c2p paymentToken: response has no webPaymentUrl")
	}
	return out.WebPaymentURL, nil
}

// Result is a payment result: the backend notification 2C2P posts to the
// backend return URL, or a Payment Inquiry answer.
type Result struct {
	MerchantID     string      `json:"merchantID"`
	InvoiceNo      string      `json:"invoiceNo"`
	Amount         json.Number `json:"amount"`
	CurrencyCode   string      `json:"currencyCode"`
	FxAmount       json.Number `json:"fxAmount"`       // set when 2C2P converted the currency
	FxCurrencyCode string      `json:"fxCurrencyCode"` // the currency we requested
	TranRef        string      `json:"tranRef"`
	ChannelCode    string      `json:"channelCode"`
	RespCode       string      `json:"respCode"`
	RespDesc       string      `json:"respDesc"`
}

// Paid reports whether the result is a successful payment.
func (n *Result) Paid() bool { return n.RespCode == RespSuccess }

// RequestedAmount is the amount and currency in the currency we asked for,
// undoing 2C2P's conversion when the merchant settles in another currency.
func (n *Result) RequestedAmount() (amount, currency string) {
	if n.FxAmount != "" && n.FxCurrencyCode != "" {
		return n.FxAmount.String(), n.FxCurrencyCode
	}
	return n.Amount.String(), n.CurrencyCode
}

// ParseNotification verifies a backend notification's JWT payload and returns
// its claims. It fails on a bad signature or another merchant's notification.
func (c *Client) ParseNotification(payload string) (*Result, error) {
	claims, err := decodeJWT(payload, c.SecretKey)
	if err != nil {
		return nil, err
	}
	var n Result
	if err := json.Unmarshal(claims, &n); err != nil {
		return nil, fmt.Errorf("2c2p notification: %w", err)
	}
	if n.MerchantID != c.MerchantID {
		return nil, fmt.Errorf("2c2p notification for merchant %q", n.MerchantID)
	}
	if n.InvoiceNo == "" {
		return nil, errors.New("2c2p notification has no invoiceNo")
	}
	return &n, nil
}

// Inquire asks 2C2P for an invoice's payment result (Payment Inquiry API).
// Only a signed answer for that invoice is returned; anything else is an
// error, so a result can be trusted like a notification.
func (c *Client) Inquire(ctx context.Context, invoiceNo string) (*Result, error) {
	claims := map[string]any{"merchantID": c.MerchantID, "invoiceNo": invoiceNo, "locale": c.locale()}
	resp, signed, err := c.postJWT(ctx, "/payment/4.3/paymentInquiry", claims)
	if err != nil {
		return nil, err
	}
	var n Result
	if err := json.Unmarshal(resp, &n); err != nil {
		return nil, fmt.Errorf("2c2p paymentInquiry: decode response: %w", err)
	}
	if !signed {
		return nil, fmt.Errorf("2c2p paymentInquiry: unsigned answer: %w", &RespError{RespCode: n.RespCode, RespDesc: n.RespDesc})
	}
	if n.InvoiceNo == "" {
		n.InvoiceNo = invoiceNo
	}
	if n.InvoiceNo != invoiceNo || n.MerchantID != "" && n.MerchantID != c.MerchantID {
		return nil, fmt.Errorf("2c2p paymentInquiry: answer for %s/%s, asked for %s/%s", n.MerchantID, n.InvoiceNo, c.MerchantID, invoiceNo)
	}
	return &n, nil
}

func (c *Client) locale() string {
	if c.Locale == "" {
		return "en"
	}
	return c.Locale
}

func (c *Client) location() *time.Location {
	if c.Location != nil {
		return c.Location
	}
	return defaultLocation
}

// postJWT sends {"payload": JWT(claims)} and returns the verified claims of the
// JWT in the answer, with signed = true. Error answers come back as plain JSON
// and are returned as-is, so their respCode can be read.
func (c *Client) postJWT(ctx context.Context, path string, claims any) (resp []byte, signed bool, err error) {
	token, err := encodeJWT(claims, c.SecretKey)
	if err != nil {
		return nil, false, err
	}
	body, _ := json.Marshal(map[string]string{"payload": token})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")

	httpResp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("2c2p %s: %w", path, err)
	}
	defer httpResp.Body.Close()
	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("2c2p %s: read response: %w", path, err)
	}
	if httpResp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("2c2p %s: HTTP %d: %s", path, httpResp.StatusCode, respBody)
	}

	var out struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, false, fmt.Errorf("2c2p %s: invalid JSON: %w", path, err)
	}
	if out.Payload == "" {
		return respBody, false, nil
	}
	claimsJSON, err := decodeJWT(out.Payload, c.SecretKey)
	if err != nil {
		return nil, false, fmt.Errorf("2c2p %s: response: %w", path, err)
	}
	return claimsJSON, true, nil
}

// formatAmount renders 10050 cents as "100.50".
func formatAmount(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// --- HS256 JWT ---

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

func encodeJWT(claims any, secret string) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(payload)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sign(signingInput, secret)), nil
}

// decodeJWT verifies the token's HS256 signature and returns its claims JSON.
func decodeJWT(token, secret string) ([]byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed JWT")
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errors.New("malformed JWT header")
	}
	var h struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(header, &h); err != nil || h.Alg != "HS256" {
		return nil, errors.New("unexpected JWT algorithm")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, sign(parts[0]+"."+parts[1], secret)) {
		return nil, errors.New("JWT signature verification failed")
	}
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("malformed JWT claims")
	}
	return claims, nil
}

func sign(signingInput, secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signingInput))
	return mac.Sum(nil)
}
