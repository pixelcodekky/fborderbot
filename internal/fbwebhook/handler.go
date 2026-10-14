package fbwebhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"fb-order-bot/internal/db"
	"fb-order-bot/internal/messenger"
	"fb-order-bot/internal/obs"
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
		obs.Logger(r.Context()).Info("facebook webhook verified")
		w.Write([]byte(q.Get("hub.challenge")))
		return
	}
	obs.Logger(r.Context()).Warn("facebook webhook verification rejected; check FB_WEBHOOK_VERIFY_TOKEN")
	w.WriteHeader(http.StatusForbidden)
}

func (h *Handler) handleEvent(w http.ResponseWriter, r *http.Request) {
	// Detached from the request so a client hang-up can't abort half-done work.
	ctx := context.WithoutCancel(r.Context())
	log := obs.Logger(ctx).With("source", "facebook")

	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		log.Warn("read body failed", "err", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if !h.validSignature(r.Header.Get("X-Hub-Signature-256"), rawBody) {
		log.Warn("invalid webhook signature; check FB_APP_SECRET")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var payload webhookPayload
	if err := json.Unmarshal(rawBody, &payload); err != nil {
		// Redelivering the same bytes won't fix it, so ACK and log.
		log.Error("bad payload", "err", err)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Process before ACKing: a non-200 makes Meta redeliver, which is how
	// transient failures get retried. Already-handled comments in the batch
	// are skipped on redelivery by the dedupe table.
	retry := false
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if change.Field != "feed" || change.Value.Item != "comment" {
				continue
			}
			if h.processComment(ctx, entry.ID, change.Value) {
				retry = true
			}
		}
	}

	if retry {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// processComment reports whether it failed in a way worth retrying. In that
// case its dedupe claim has been released so Meta's redelivery runs it again.
func (h *Handler) processComment(ctx context.Context, pageFBID string, c commentValue) (retry bool) {
	if c.Verb != "add" {
		return false // ignore edits/removes for order-triggering purposes
	}
	if !strings.Contains(strings.ToLower(c.Message), h.TriggerKeyword) {
		return false
	}

	log := obs.Logger(ctx).With(
		"source", "facebook",
		"page_fb_id", pageFBID,
		"comment_id", c.CommentID,
		"post_id", c.PostID,
	)

	// Dedupe: Meta redelivers webhooks, and a Page can receive the same
	// comment event more than once.
	first, err := db.MarkEventSeen(ctx, h.Pool, "facebook", c.CommentID)
	if err != nil {
		log.Error("dedupe check failed", "step", "dedupe", "err", err)
		return true
	}
	if !first {
		log.Info("duplicate comment event skipped")
		return false
	}

	release := func() {
		if err := db.ReleaseEvent(ctx, h.Pool, "facebook", c.CommentID); err != nil {
			log.Error("release dedupe claim failed", "err", err)
		}
	}

	pageID, pageAccessToken, err := h.lookupPage(ctx, pageFBID)
	if errors.Is(err, pgx.ErrNoRows) {
		log.Error("comment for unknown page; add it to the pages table", "step", "lookup_page")
		return false
	}
	if err != nil {
		log.Error("page lookup failed", "step", "lookup_page", "err", err)
		release()
		return true
	}
	log = log.With("page_id", pageID)

	// Create the order row immediately in pending_form status, keyed by the
	// comment ID. On a retry the row already exists and we just get its id.
	var orderID int64
	err = h.Pool.QueryRow(ctx,
		`INSERT INTO orders (page_id, fb_comment_id, status)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (fb_comment_id) DO UPDATE SET updated_at = now()
		 RETURNING id`,
		pageID, c.CommentID, "pending_form",
	).Scan(&orderID)
	if err != nil {
		log.Error("create order failed", "step", "create_order", "err", err)
		release()
		return true
	}
	log = log.With("order_id", orderID)
	log.Info("order created")

	client := h.NewMessenger(pageAccessToken)
	psid, err := client.SendPrivateReply(c.CommentID,
		"Hey! Thanks for your interest 🙌 Tap below to pick your product and place your order.")
	if err != nil {
		h.recordOrderError(ctx, log, orderID, "private_reply", err)
		if messenger.IsPermanent(err) {
			return false
		}
		release()
		return true
	}
	log = log.With("psid", psid)

	if _, err := h.Pool.Exec(ctx,
		`UPDATE orders
		 SET psid = $1, last_error = NULL, last_error_step = NULL, last_error_at = NULL, updated_at = now()
		 WHERE id = $2`,
		psid, orderID,
	); err != nil {
		// The private reply already went out and can only be sent once per
		// comment, so a retry can't help. The psid is in this log line so it
		// can be patched in by hand.
		h.recordOrderError(ctx, log, orderID, "store_psid", err)
		return false
	}
	log.Info("private reply sent")

	// From here, a follow-up call (not shown) would send the product
	// carousel to psid via client.SendProductCarousel(...).
	return false
}

func (h *Handler) recordOrderError(ctx context.Context, log *slog.Logger, orderID int64, step string, err error) {
	log.Error(step+" failed", "step", step, "err", err)
	if dbErr := db.RecordOrderError(ctx, h.Pool, orderID, step, err); dbErr != nil {
		log.Error("record order error failed", "err", dbErr)
	}
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
