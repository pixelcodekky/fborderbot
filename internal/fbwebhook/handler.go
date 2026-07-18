package fbwebhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"fb-order-bot/internal/db"
	"fb-order-bot/internal/messenger"
)

type Handler struct {
	Pool           *pgxpool.Pool
	VerifyToken    string // arbitrary string you choose, set in the FB App dashboard
	AppSecret      string // used to validate X-Hub-Signature-256
	TriggerKeyword string // e.g. "order"
	NewMessenger   func(pageAccessToken string) *messenger.Client
}

// ServeHTTP handles both the one-time GET verification handshake and the
// ongoing POST event deliveries on the same route, per Meta's webhook spec.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.handleVerify(w, r)
	case http.MethodPost:
		h.handleEvent(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleVerify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("hub.mode") == "subscribe" && q.Get("hub.verify_token") == h.VerifyToken {
		w.Write([]byte(q.Get("hub.challenge")))
		return
	}
	w.WriteHeader(http.StatusForbidden)
}

func (h *Handler) handleEvent(w http.ResponseWriter, r *http.Request) {
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if !h.validSignature(r.Header.Get("X-Hub-Signature-256"), rawBody) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Always ACK fast — Meta retries aggressively on non-200s. Do the real
	// work after responding, or hand off to a queue in production.
	w.WriteHeader(http.StatusOK)

	var payload webhookPayload
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		log.Printf("fbwebhook: bad payload: %v", err)
		return
	}

	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if change.Field != "feed" || change.Value.Item != "comment" {
				continue
			}
			h.processComment(entry.ID, change.Value)
		}
	}
}

func (h *Handler) processComment(pageFBID string, c commentValue) {
	if c.Verb != "add" {
		return // ignore edits/removes for order-triggering purposes
	}
	if !strings.Contains(strings.ToLower(c.Message), h.TriggerKeyword) {
		return
	}

	ctx := context.Background()

	// Dedupe: Meta redelivers webhooks, and a Page can receive the same
	// comment event more than once.
	first, err := db.MarkEventSeen(ctx, h.Pool, "facebook", c.CommentID)
	if err != nil {
		log.Printf("fbwebhook: dedupe check failed: %v", err)
		return
	}
	if !first {
		return
	}

	pageID, pageAccessToken, err := h.lookupPage(ctx, pageFBID)
	if err != nil {
		log.Printf("fbwebhook: unknown page %s: %v", pageFBID, err)
		return
	}

	// Create the order row immediately in pending_form status, keyed by the
	// comment ID (unique constraint means retries are naturally idempotent).
	_, err = h.Pool.Exec(ctx,
		`INSERT INTO orders (page_id, fb_comment_id, status)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (fb_comment_id) DO NOTHING`,
		pageID, c.CommentID, "pending_form",
	)
	if err != nil {
		log.Printf("fbwebhook: failed to create order: %v", err)
		return
	}

	client := h.NewMessenger(pageAccessToken)
	psid, err := client.SendPrivateReply(c.CommentID,
		"Hey! Thanks for your interest 🙌 Tap below to pick your product and place your order.")
	if err != nil {
		log.Printf("fbwebhook: private reply failed for comment %s: %v", c.CommentID, err)
		return
	}

	if _, err := h.Pool.Exec(ctx,
		`UPDATE orders SET psid = $1, updated_at = now() WHERE fb_comment_id = $2`,
		psid, c.CommentID,
	); err != nil {
		log.Printf("fbwebhook: failed to store psid: %v", err)
	}

	// From here, a follow-up call (not shown) would send the product
	// carousel to psid via client.SendProductCarousel(...).
}

func (h *Handler) lookupPage(ctx context.Context, fbPageID string) (pageID int64, accessToken string, err error) {
	err = h.Pool.QueryRow(ctx,
		`SELECT id, page_access_token FROM pages WHERE fb_page_id = $1`,
		fbPageID,
	).Scan(&pageID, &accessToken)
	return
}

func (h *Handler) validSignature(header string, body []byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	expectedHex := strings.TrimPrefix(header, prefix)
	expected, err := hex.DecodeString(expectedHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(h.AppSecret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), expected)
}

// --- payload shapes (trimmed to the fields we use) ---

type webhookPayload struct {
	Entry []struct {
		ID      string         `json:"id"`
		Changes []changeEntry  `json:"changes"`
	} `json:"entry"`
}

type changeEntry struct {
	Field string       `json:"field"`
	Value commentValue `json:"value"`
}

type commentValue struct {
	Item      string `json:"item"`
	Verb      string `json:"verb"`
	CommentID string `json:"comment_id"`
	Message   string `json:"message"`
	PostID    string `json:"post_id"`
}
