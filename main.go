package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/joho/godotenv"

	"github.com/stripe/stripe-go/v78"

	"fb-order-bot/internal/db"
	"fb-order-bot/internal/fbwebhook"
	"fb-order-bot/internal/messenger"
	"fb-order-bot/internal/stripewebhook"
)

func main() {
	godotenv.Load()
	ctx := context.Background()

	stripe.Key = os.Getenv("STRIPE_SECRET_KEY")

	pool, err := db.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("db connect failed: %v", err)
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
	mux.Handle("/health", http.HandlerFunc(health))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("listening on %s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func sendEmailStub(to, subject, body string) error {
	log.Printf("[email stub] to=%s subject=%s", to, subject)
	return nil
}

func health(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
