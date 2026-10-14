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

	"fb-order-bot/internal/db"
	"fb-order-bot/internal/fbwebhook"
	"fb-order-bot/internal/messenger"
	"fb-order-bot/internal/obs"
	"fb-order-bot/internal/stripewebhook"
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
	log := obs.Setup(os.Getenv("LOG_LEVEL"))
	ctx := context.Background()

	var missing []string
	for _, k := range requiredEnv {
		if os.Getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		log.Error("missing required env vars", "missing", missing)
		os.Exit(1)
	}

	stripe.Key = os.Getenv("STRIPE_SECRET_KEY")

	pool, err := db.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Error("db connect failed", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	fbHandler := &fbwebhook.Handler{
		Pool:           pool,
		VerifyToken:    os.Getenv("FB_WEBHOOK_VERIFY_TOKEN"),
		AppSecret:      os.Getenv("FB_APP_SECRET"),
		TriggerKeyword: "order",
		NewMessenger:   messenger.New,
	}

	stripeHandler := &stripewebhook.Handler{
		Pool:          pool,
		WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
		SendEmail:     sendEmailStub, // swap in SES/SendGrid
	}

	mux := http.NewServeMux()
	mux.Handle("/webhooks/facebook", fbHandler)
	mux.Handle("/webhooks/stripe", stripeHandler)
	mux.Handle("/health", health(pool))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Info("listening", "port", port)
	if err := http.ListenAndServe(":"+port, obs.Middleware(mux)); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
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
