package fbwebhook

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"fb-order-bot/internal/obs"
)

// postItems are the feed "item" values Meta sends for a Page's own posts.
var postItems = map[string]bool{
	"status": true,
	"photo":  true,
	"video":  true,
	"post":   true,
}

// processPost keeps trigger_posts in sync with the Page's posts: the post text
// is scanned for the page's trigger_keywords, and each keyword found gets a
// (page, post, keyword) row, inserted or updated. A deleted post stops being
// watched. Both writes are idempotent, so it reports retry on any database error.
func (h *Handler) processPost(ctx context.Context, pageFBID string, p commentValue) (retry bool) {
	if p.From.ID != "" && p.From.ID != pageFBID {
		return false // visitor posts on the Page can't become trigger posts
	}

	log := obs.Logger(ctx).With(
		"source", "facebook",
		"page_fb_id", pageFBID,
		"post_id", p.PostID,
		"item", p.Item,
		"verb", p.Verb,
	)
	if p.PostID == "" {
		log.Warn("post event without post_id; skipped")
		return false
	}

	switch p.Verb {
	case "add", "edited":
		if p.Message == "" {
			log.Info("post has no text; not watched")
			return false
		}
		pageID, keywords, err := h.pageKeywords(ctx, pageFBID)
		if errors.Is(err, pgx.ErrNoRows) {
			log.Error("post for unknown page; add it to the pages table", "step", "load_keywords")
			return false
		}
		if err != nil {
			log.Error("load trigger keywords failed", "step", "load_keywords", "err", err)
			return true
		}
		log = log.With("page_id", pageID)

		found := findKeywords(p.Message, keywords)
		if len(found) == 0 {
			// An edit that drops a keyword leaves existing rows alone, so
			// posts added by hand aren't switched off by accident.
			log.Info("post has no trigger keyword; not watched", "keywords", keywords)
			return false
		}
		if _, err := h.Pool.Exec(ctx,
			`INSERT INTO trigger_posts (page_id, fb_post_id, keyword, post_message, source)
			 SELECT $1, $2, k, $4, 'auto' FROM unnest($3::text[]) AS k
			 ON CONFLICT (page_id, fb_post_id, keyword) DO UPDATE
			     SET active = true, post_message = EXCLUDED.post_message, updated_at = now()`,
			pageID, p.PostID, found, p.Message,
		); err != nil {
			log.Error("save trigger post failed", "step", "save_trigger_post", "err", err)
			return true
		}
		log.Info("trigger post saved", "keywords", found)

	case "remove":
		if _, err := h.Pool.Exec(ctx,
			`UPDATE trigger_posts SET active = false, updated_at = now()
			 WHERE fb_post_id = $1 AND page_id = (SELECT id FROM pages WHERE fb_page_id = $2)`,
			p.PostID, pageFBID,
		); err != nil {
			log.Error("deactivate trigger post failed", "step", "deactivate_trigger_post", "err", err)
			return true
		}
		log.Info("post removed; trigger post deactivated")
	}
	return false
}

// pageKeywords returns the page's id and its active trigger keywords. It
// returns pgx.ErrNoRows only when the page itself is unknown.
func (h *Handler) pageKeywords(ctx context.Context, fbPageID string) (pageID int64, keywords []string, err error) {
	err = h.Pool.QueryRow(ctx,
		`SELECT p.id, array_remove(array_agg(k.keyword ORDER BY k.keyword), NULL)
		 FROM pages p
		 LEFT JOIN trigger_keywords k ON k.page_id = p.id AND k.active
		 WHERE p.fb_page_id = $1
		 GROUP BY p.id`,
		fbPageID,
	).Scan(&pageID, &keywords)
	return
}

// findKeywords returns the keywords that appear in message, ignoring case,
// in the order given.
func findKeywords(message string, keywords []string) []string {
	var found []string
	for _, k := range keywords {
		if matchesKeyword(message, k) {
			found = append(found, k)
		}
	}
	return found
}
