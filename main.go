package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/stripe/stripe-go/v78"
	"github.com/stripe/stripe-go/v78/checkout/session"

	"fb-order-bot/internal/db"
	"fb-order-bot/internal/fbwebhook"
	"fb-order-bot/internal/messenger"
	"fb-order-bot/internal/obs"
	"fb-order-bot/internal/payments"
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

	// The PayNow button: Stripe Checkout, which needs somewhere to send the
	// customer back to. The result comes back on /webhooks/stripe.
	if successURL, cancelURL := os.Getenv("CHECKOUT_SUCCESS_URL"), os.Getenv("CHECKOUT_CANCEL_URL"); successURL != "" && cancelURL != "" {
		expiry, err := time.ParseDuration(envOr("STRIPE_CHECKOUT_EXPIRY", "30m"))
		if err != nil || expiry < 30*time.Minute || expiry > 24*time.Hour {
			fatal("bad STRIPE_CHECKOUT_EXPIRY; Stripe allows 30m to 24h", "err", err)
		}
		pay.Providers[payments.MethodPayNow] = &payments.Stripe{
			Sessions:   session.Client{B: stripe.GetBackend(stripe.APIBackend), Key: stripe.Key},
			Methods:    []string{"paynow"},
			SuccessURL: successURL,
			CancelURL:  cancelURL,
			PageExpiry: expiry,
		}
	} else {
		log.Warn("CHECKOUT_SUCCESS_URL / CHECKOUT_CANCEL_URL not set; the PayNow button can't create a payment link")
	}

	// The 2C2P button: a hosted payment page; the result comes back as a
	// signed notification on /webhooks/2c2p.
	var twoc2pHandler *twoc2pwebhook.Handler
	if merchantID, secretKey := os.Getenv("TWOC2P_MERCHANT_ID"), os.Getenv("TWOC2P_SECRET_KEY"); merchantID != "" && secretKey != "" {
		frontendURL, backendURL := os.Getenv("TWOC2P_FRONTEND_RETURN_URL"), os.Getenv("TWOC2P_BACKEND_RETURN_URL")
		if frontendURL == "" || backendURL == "" {
			fatal("TWOC2P_FRONTEND_RETURN_URL and TWOC2P_BACKEND_RETURN_URL are required with TWOC2P_MERCHANT_ID")
		}
		client := twoc2p.New(envOr("TWOC2P_BASE_URL", twoc2p.SandboxBaseURL), merchantID, secretKey, frontendURL, backendURL)
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
		twoc2pHandler = &twoc2pwebhook.Handler{Pool: pool, Client: client, Payments: pay, NewMessenger: messenger.New}
	} else {
		log.Warn("TWOC2P_MERCHANT_ID / TWOC2P_SECRET_KEY not set; the 2C2P button can't create a payment link")
	}

	stripeHandler := &stripewebhook.Handler{
		Pool:          pool,
		WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
		Payments:      pay,
		NewMessenger:  messenger.New,
		SendEmail:     sendEmailStub, // swap in SES/SendGrid
	}

	mux := http.NewServeMux()
	mux.Handle("/webhooks/facebook", fbHandler)
	mux.Handle("/webhooks/stripe", stripeHandler)
	if twoc2pHandler != nil {
		mux.Handle("/webhooks/2c2p", twoc2pHandler)
		mux.HandleFunc("/payments/2c2p/return", twoc2pHandler.ReturnPage)
	}
	mux.Handle("/health", health(pool))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Info("listening", "port", port)
	if err := http.ListenAndServe(":"+port, obs.Middleware(mux)); err != nil {
		fatal("server stopped", "err", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
