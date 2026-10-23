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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"fb-order-bot/internal/db"
	"fb-order-bot/internal/messenger"
	"fb-order-bot/internal/obs"
	"fb-order-bot/internal/payments"
)

type Handler struct {
	Pool           *pgxpool.Pool
	VerifyToken    string // arbitrary string you choose, set in the FB App dashboard
	AppSecret      string // used to validate X-Hub-Signature-256
	TriggerKeyword string // e.g. "order"
	NewMessenger   func(pageAccessToken string) *messenger.Client

	// Payments sets order quantities and makes the payment links for the
	// 2C2P and Stripe Payment buttons; see payments.Service. Nil disables both.
	Payments OrderPayments

	Now func() time.Time // defaults to time.Now; overridden in tests
}

// OrderPayments is implemented by *payments.Service.
type OrderPayments interface {
	Pay(ctx context.Context, method string, req payments.PayRequest) (payments.PayResult, error)
	SetQuantity(ctx context.Context, orderID int64, qty int) (amountCents int64, res payments.PayResult, err error)
}

// privateReplyWindow is how long after a comment Meta still accepts a
// private reply to it.
const privateReplyWindow = 7 * 24 * time.Hour

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
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

	// Debug only: bodies hold commenters' names and messages. Run with
	// LOG_LEVEL=debug to see them.
	log.Debug("webhook body", "body", string(rawBody))

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
	if payload.Object != "page" {
		log.Info("ignoring non-page webhook", "object", payload.Object)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Process before ACKing: a non-200 makes Meta redeliver, which is how
	// transient failures get retried. Already-handled comments in the batch
	// are skipped on redelivery by the dedupe table; post handling is an
	// idempotent upsert.
	retry := false
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if change.Field != "feed" {
				continue
			}
			var failed bool
			switch {
			case change.Value.Item == "comment":
				log.Info("Processing comment", "comment", change.Value)
				failed = h.processComment(ctx, entry.ID, change.Value)
			case postItems[change.Value.Item]:
				log.Info("Processing post", "post", change.Value)
				failed = h.processPost(ctx, entry.ID, change.Value)
			}
			if failed {
				retry = true
			}
		}
		for _, ev := range entry.Messaging {
			var failed bool
			switch {
			case ev.Postback != nil:
				failed = h.processPostback(ctx, entry.ID, ev)
			case ev.Message != nil:
				failed = h.processMessage(ctx, entry.ID, ev)
			}
			if failed {
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
	if c.From.ID == pageFBID {
		return false // the Page's own replies must not trigger orders
	}

	log := obs.Logger(ctx).With(
		"source", "facebook",
		"page_fb_id", pageFBID,
		"comment_id", c.CommentID,
		"post_id", c.PostID,
		"from_id", c.From.ID,
	)

	// Meta leaves out "message" when the app can't read the commenter's
	// content (missing pages_read_user_content, or a non-tester while the
	// app is in development mode), and for sticker/photo-only comments.
	if c.Message == "" {
		log.Info("comment has no message text; skipped")
		return false
	}
	if c.Post.ID != "" && !c.Post.IsPublished {
		log.Info("comment on unpublished post; skipped")
		return false
	}
	// A private reply is only allowed within 7 days of the comment, so a
	// late redelivery would fail permanently anyway.
	if age := h.now().Sub(time.Unix(c.CreatedTime, 0)); c.CreatedTime != 0 && age > privateReplyWindow {
		log.Warn("comment too old for a private reply; skipped", "age", age.String())
		return false
	}

	t, err := h.lookupTrigger(ctx, pageFBID, c.PostID)
	if errors.Is(err, pgx.ErrNoRows) {
		log.Error("comment for unknown page; add it to the pages table", "step", "lookup_page")
		return false
	}
	if err != nil {
		log.Error("page lookup failed", "step", "lookup_page", "err", err)
		return true
	}
	log = log.With("page_id", t.pageID)
	if !t.postWatched {
		// The permalink is logged so the post can be found and added.
		log.Info("post is not a trigger post; skipped", "permalink", c.Post.PermalinkURL)
		return false
	}

	keywords := make([]string, len(t.keywords))
	for i, k := range t.keywords {
		if k == "" {
			k = h.TriggerKeyword // manual row without its own keyword
		}
		keywords[i] = k
	}
	matched := findKeywords(c.Message, keywords)
	if len(matched) == 0 {
		log.Info("comment has no trigger keyword; skipped", "keywords", keywords)
		return false
	}
	log = log.With("keyword", matched[0])

	// Without a linked product there is no price, so the reply falls back to
	// plain text and the order has no amount.
	prod, err := h.lookupProduct(ctx, t.pageID, matched)
	if errors.Is(err, pgx.ErrNoRows) {
		log.Warn("no active product linked to keyword; replying without payment buttons")
	} else if err != nil {
		log.Error("product lookup failed", "step", "lookup_product", "err", err)
		return true
	} else {
		log = log.With("product_id", prod.productID)
	}

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

	pageID, pageAccessToken := t.pageID, t.accessToken

	// Create the order row immediately in pending_form status, keyed by the
	// comment ID. On a retry the row already exists and we just get its id.
	var orderID int64
	// Quantity starts at 1; the customer picks it next (see setQuantity).
	var productID, amountCents *int64
	if prod.productID != 0 {
		productID, amountCents = &prod.productID, &prod.priceCents
	}
	err = h.Pool.QueryRow(ctx,
		`INSERT INTO orders (page_id, fb_comment_id, status,
		                     commenter_fb_id, commenter_name, comment_message, post_permalink,
		                     product_id, unit_price_cents, amount_cents, currency)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9, coalesce($10, 'sgd'))
		 ON CONFLICT (fb_comment_id) DO UPDATE SET updated_at = now()
		 RETURNING id`,
		pageID, c.CommentID, "pending_form",
		nullIfEmpty(c.From.ID), nullIfEmpty(c.From.Name), nullIfEmpty(c.Message), nullIfEmpty(c.Post.PermalinkURL),
		productID, amountCents, nullIfEmpty(prod.currency),
	).Scan(&orderID)
	if err != nil {
		log.Error("create order failed", "step", "create_order", "err", err)
		release()
		return true
	}
	log = log.With("order_id", orderID)
	log.Info("order created")

	client := h.NewMessenger(pageAccessToken)
	var psid string
	if prod.productID != 0 {
		psid, err = client.SendPrivateReplyButtons(c.CommentID,
			quantityPromptText(prod.keyword, prod.priceCents, prod.currency), quantityButtons(orderID))
	} else {
		psid, err = client.SendPrivateReply(c.CommentID,
			"Hey! Thanks for your interest 🙌 We'll get back to you with the details shortly.")
	}
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
	log.Info("private reply sent", "permalink", c.Post.PermalinkURL)

	// The customer's quantity and payment taps arrive as postbacks; see
	// processPostback.
	return false
}

func (h *Handler) recordOrderError(ctx context.Context, log *slog.Logger, orderID int64, step string, err error) {
	log.Error(step+" failed", "step", step, "err", err)
	if dbErr := db.RecordOrderError(ctx, h.Pool, orderID, step, err); dbErr != nil {
		log.Error("record order error failed", "err", dbErr)
	}
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type trigger struct {
	pageID      int64
	accessToken string
	postWatched bool     // the post has at least one active trigger_posts row
	keywords    []string // one per row; "" means use Handler.TriggerKeyword
}

// lookupTrigger loads the page and the post's active trigger rows. It returns
// pgx.ErrNoRows only when the page itself is unknown.
func (h *Handler) lookupTrigger(ctx context.Context, fbPageID, fbPostID string) (t trigger, err error) {
	err = h.Pool.QueryRow(ctx,
		`SELECT p.id, p.page_access_token, count(tp.id) > 0,
		        coalesce(array_agg(coalesce(tp.keyword, '') ORDER BY tp.keyword)
		                 FILTER (WHERE tp.id IS NOT NULL), '{}')
		 FROM pages p
		 LEFT JOIN trigger_posts tp
		        ON tp.page_id = p.id AND tp.fb_post_id = $2 AND tp.active
		 WHERE p.fb_page_id = $1
		 GROUP BY p.id, p.page_access_token`,
		fbPageID, fbPostID,
	).Scan(&t.pageID, &t.accessToken, &t.postWatched, &t.keywords)
	return
}

func matchesKeyword(message, keyword string) bool {
	return keyword != "" && strings.Contains(strings.ToLower(message), strings.ToLower(keyword))
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
	Object string `json:"object"` // "page"
	Entry  []struct {
		ID        string           `json:"id"`        // Page ID
		Time      int64            `json:"time"`      // unix seconds
		Changes   []changeEntry    `json:"changes"`   // feed: posts and comments
		Messaging []messagingEvent `json:"messaging"` // Messenger: button taps and messages
	} `json:"entry"`
}

type messagingEvent struct {
	Sender struct {
		ID string `json:"id"` // customer's PSID
	} `json:"sender"`
	Recipient struct {
		ID string `json:"id"` // Page ID
	} `json:"recipient"`
	Timestamp int64 `json:"timestamp"` // unix milliseconds
	Postback  *struct {
		MID     string `json:"mid"`
		Title   string `json:"title"`
		Payload string `json:"payload"`
	} `json:"postback"`
	Message *struct {
		MID        string `json:"mid"`
		Text       string `json:"text"`
		IsEcho     bool   `json:"is_echo"` // sent by the Page
		QuickReply *struct {
			Payload string `json:"payload"`
		} `json:"quick_reply"`
	} `json:"message"`
}

type changeEntry struct {
	Field string       `json:"field"`
	Value commentValue `json:"value"`
}

// commentValue is a feed change's "value". Despite the name it is also used
// for post changes, which share item/verb/post_id/message/from.
type commentValue struct {
	Item        string `json:"item"`
	Verb        string `json:"verb"`
	CommentID   string `json:"comment_id"`
	PostID      string `json:"post_id"`
	ParentID    string `json:"parent_id"` // post ID for top-level comments, comment ID for replies
	CreatedTime int64  `json:"created_time"`
	Message     string `json:"message"` // may be absent, see processComment
	From        struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"from"`
	Post postInfo `json:"post"` // sent alongside comments; may be absent
}

type postInfo struct {
	ID              string `json:"id"`
	StatusType      string `json:"status_type"` // e.g. "mobile_status_update"
	IsPublished     bool   `json:"is_published"`
	UpdatedTime     string `json:"updated_time"` // e.g. "2026-10-20T01:31:46+0000"
	PermalinkURL    string `json:"permalink_url"`
	PromotionStatus string `json:"promotion_status"`
}
