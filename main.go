package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/stripe/stripe-go/v78"
	"github.com/stripe/stripe-go/v78/checkout/session"

	"fb-order-bot/internal/db"
	"fb-order-bot/internal/fbwebhook"
	"fb-order-bot/internal/messenger"
	"fb-order-bot/internal/notify"
	"fb-order-bot/internal/obs"
	"fb-order-bot/internal/paymentreturn"
	"fb-order-bot/internal/payments"
	"fb-order-bot/internal/ratelimit"
	"fb-order-bot/internal/receipt"
	"fb-order-bot/internal/stripewebhook"
	"fb-order-bot/internal/twoc2p"
	"fb-order-bot/internal/twoc2pwebhook"
)

var requiredEnv = []string{
	"DATABASE_URL",
	"FB_WEBHOOK_VERIFY_TOKEN",
	"FB_APP_SECRET",
	"STRIPE_SECRET_KEY",
	"STRIPE_WEBHOOK_SECRET",
}

func main() {
	godotenv.Load()
	log := obs.Setup(obs.Options{
		Level:             os.Getenv("LOG_LEVEL"),
		SentryDSN:         os.Getenv("SENTRY_DSN"),
		SentryEnvironment: envOr("SENTRY_ENVIRONMENT", "production"),
		Release:           os.Getenv("RENDER_GIT_COMMIT"), // set by Render
	})
	defer obs.Flush()
	ctx := context.Background()

	fatal := func(msg string, args ...any) {
		log.Error(msg, args...)
		obs.Flush() // os.Exit skips deferred calls
		os.Exit(1)
	}

	var missing []string
	for _, k := range requiredEnv {
		if os.Getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		fatal("missing required env vars", "missing", missing)
	}

	stripe.Key = os.Getenv("STRIPE_SECRET_KEY")

	pool, err := db.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		fatal("db connect failed", "err", err)
	}
	defer pool.Close()

	fbHandler := &fbwebhook.Handler{
		Pool:           pool,
		VerifyToken:    os.Getenv("FB_WEBHOOK_VERIFY_TOKEN"),
		AppSecret:      os.Getenv("FB_APP_SECRET"),
		TriggerKeyword: "order",
		NewMessenger:   messenger.New,
	}
	// Both payment buttons go through one service, so an order has one open
	// payment page at a time across methods.
	pay := &payments.Service{Pool: pool, Providers: map[string]payments.Provider{}}
	fbHandler.Payments = pay

	// Receipt links in payment confirmations need the bot's public address.
	publicURL := publicBaseURL()
	if publicURL == "" {
		log.Warn("PUBLIC_BASE_URL not set; payment confirmations won't include a receipt link")
	}
	// Receipt times are shown in the shop's zone.
	shopTZ, err := time.LoadLocation(envOr("SHOP_TIMEZONE", "Asia/Singapore"))
	if err != nil {
		fatal("bad SHOP_TIMEZONE", "err", err)
	}

	// Every payment method sends the customer's browser back to one page,
	// which shows paid / failed / cancelled. Stripe's return links are signed
	// (a cancel closes the Checkout Session), with a key derived from the
	// webhook secret so there's no extra secret to manage.
	returnURL := os.Getenv("PAYMENT_RETURN_URL")
	if returnURL == "" && publicURL != "" {
		returnURL = publicURL + "/payments/return"
	}
	for _, k := range []string{"CHECKOUT_SUCCESS_URL", "CHECKOUT_CANCEL_URL", "TWOC2P_FRONTEND_RETURN_URL"} {
		if os.Getenv(k) != "" {
			log.Warn(k+" is no longer used; every payment returns to PAYMENT_RETURN_URL", "payment_return_url", returnURL)
		}
	}
	returnKey := []byte(os.Getenv("STRIPE_WEBHOOK_SECRET"))
	notifier := &notify.Customer{
		Pool:          pool,
		NewMessenger:  messenger.New,
		PublicBaseURL: publicURL,
		SendEmail:     sendEmailStub, // swap in SES/SendGrid
	}

	// The Stripe Payment button: Stripe Checkout offering card and PayNow.
	// The result comes back on /webhooks/stripe.
	if returnURL != "" {
		expiry, err := time.ParseDuration(envOr("STRIPE_CHECKOUT_EXPIRY", "30m"))
		if err != nil || expiry < 30*time.Minute || expiry > 24*time.Hour {
			fatal("bad STRIPE_CHECKOUT_EXPIRY; Stripe allows 30m to 24h", "err", err)
		}
		sessions := session.Client{B: stripe.GetBackend(stripe.APIBackend), Key: stripe.Key}
		checkout := &payments.Stripe{Sessions: sessions, Methods: []string{"card", "paynow"}, ReturnURL: returnURL, ReturnKey: returnKey, PageExpiry: expiry}
		pay.Providers[payments.MethodStripe] = checkout
		// Settles PayNow-only attempts made before; no button makes new ones.
		pay.Providers[payments.MethodPayNow] = checkout
	} else {
		log.Warn("PUBLIC_BASE_URL / PAYMENT_RETURN_URL not set; the Stripe Payment button can't create a payment link")
	}

	// The 2C2P button: a hosted payment page; the result comes back as a
	// signed notification on /webhooks/2c2p.
	var twoc2pHandler *twoc2pwebhook.Handler
	if merchantID, secretKey := os.Getenv("TWOC2P_MERCHANT_ID"), os.Getenv("TWOC2P_SECRET_KEY"); merchantID != "" && secretKey != "" {
		backendURL := os.Getenv("TWOC2P_BACKEND_RETURN_URL")
		if backendURL == "" || returnURL == "" {
			fatal("TWOC2P_BACKEND_RETURN_URL and PUBLIC_BASE_URL (or PAYMENT_RETURN_URL) are required with TWOC2P_MERCHANT_ID")
		}
		client := twoc2p.New(envOr("TWOC2P_BASE_URL", twoc2p.SandboxBaseURL), merchantID, secretKey, returnURL, backendURL)
		// paymentExpiry is sent in this zone; it must match how 2C2P reads it.
		loc, err := time.LoadLocation(envOr("TWOC2P_TIMEZONE", "Asia/Singapore"))
		if err != nil {
			fatal("bad TWOC2P_TIMEZONE", "err", err)
		}
		client.Location = loc
		expiry, err := time.ParseDuration(envOr("TWOC2P_PAYMENT_EXPIRY", "20m"))
		if err != nil || expiry < time.Minute {
			fatal("bad TWOC2P_PAYMENT_EXPIRY; use e.g. 20m", "err", err)
		}
		pay.Providers[payments.Method2C2P] = &payments.TwoC2P{Client: client, PageExpiry: expiry}
		twoc2pHandler = &twoc2pwebhook.Handler{Pool: pool, Client: client, Payments: pay, Notify: notifier}
	} else {
		log.Warn("TWOC2P_MERCHANT_ID / TWOC2P_SECRET_KEY not set; the 2C2P button can't create a payment link")
	}

	stripeHandler := &stripewebhook.Handler{
		Pool:          pool,
		WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
		Payments:      pay,
		Notify:        notifier,
	}
	returnPage := &paymentreturn.Handler{Payments: pay, ReturnKey: returnKey, Notify: notifier}

	mux := http.NewServeMux()
	mux.Handle("/webhooks/facebook", fbHandler)
	mux.Handle("/webhooks/stripe", stripeHandler)
	if twoc2pHandler != nil {
		mux.Handle("/webhooks/2c2p", twoc2pHandler)
	}
	// Pages anyone with a link can open are rate limited; the webhooks are
	// not, as they reject bad signatures cheaply and the providers' bursts
	// must not be refused.
	limiter := &ratelimit.Limiter{
		PerMinute:   envFloat("RATE_LIMIT_PER_MINUTE", 30, fatal),
		Burst:       envInt("RATE_LIMIT_BURST", 10, fatal),
		MaxInFlight: envInt("RATE_LIMIT_MAX_IN_FLIGHT", 20, fatal),
		ProxyHops:   envInt("TRUSTED_PROXY_HOPS", 1, fatal),
	}
	if limiter.PerMinute <= 0 || limiter.Burst < 1 {
		fatal("RATE_LIMIT_PER_MINUTE and RATE_LIMIT_BURST must be positive")
	}
	public := limiter.Wrap
	// If PAYMENT_RETURN_URL points elsewhere, that page must forward here.
	mux.Handle("/payments/return", public(returnPage))
	// The old 2C2P-only path, for pages made before the common one.
	mux.Handle("/payments/2c2p/return", public(returnPage))
	mux.Handle("GET /receipts/{token}", public(&receipt.Handler{Payments: pay, Location: shopTZ}))
	mux.Handle("/health", health(pool))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{
		Addr:    ":" + port,
		Handler: obs.Middleware(mux),
		// A client can't hold a connection open by sending slowly. No write
		// timeout: a webhook may take a while to handle, and cutting it off
		// would only make the provider redeliver.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
	log.Info("listening", "port", port)
	if err := srv.ListenAndServe(); err != nil {
		fatal("server stopped", "err", err)
	}
}

// publicBaseURL is PUBLIC_BASE_URL, or else the origin of
// TWOC2P_BACKEND_RETURN_URL, which must be this bot too.
func publicBaseURL() string {
	if v := os.Getenv("PUBLIC_BASE_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	if u, err := url.Parse(os.Getenv("TWOC2P_BACKEND_RETURN_URL")); err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return ""
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int, fatal func(string, ...any)) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		fatal("bad "+key+"; want a whole number", "value", v)
	}
	return n
}

func envFloat(key string, fallback float64, fatal func(string, ...any)) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		fatal("bad "+key+"; want a number", "value", v)
	}
	return f
}

func sendEmailStub(to, subject, body string) error {
	slog.Info("email stub", "to", to, "subject", subject)
	return nil
}

// health reports 503 when the database is unreachable, so the platform's
// health check restarts or alerts instead of seeing a green app that can't work.
func health(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			obs.Logger(r.Context()).Error("health check: database unreachable", "err", err)
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}
}
