package fbwebhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

const testSecret = "test-app-secret"

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func loadSample(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/comment_add_no_message.json")
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestPayloadParsesSample(t *testing.T) {
	var p webhookPayload
	if err := json.Unmarshal(loadSample(t), &p); err != nil {
		t.Fatal(err)
	}
	if p.Object != "page" || len(p.Entry) != 1 || len(p.Entry[0].Changes) != 1 {
		t.Fatalf("unexpected shape: %+v", p)
	}
	e := p.Entry[0]
	c := e.Changes[0]
	if e.ID != "1267866303069580" || c.Field != "feed" {
		t.Fatalf("entry: %+v", e)
	}
	v := c.Value
	if v.Item != "comment" || v.Verb != "add" ||
		v.CommentID != "122117643123405840_1677940407452817" ||
		v.PostID != "1267866303069580_122117643123405840" ||
		v.ParentID != v.PostID ||
		v.From.ID != "28768664042804007" ||
		v.CreatedTime != 1792443298 ||
		v.Message != "" {
		t.Fatalf("value: %+v", v)
	}
}

func TestPayloadParsesSampleWithMessage(t *testing.T) {
	body, err := os.ReadFile("testdata/comment_add.json")
	if err != nil {
		t.Fatal(err)
	}
	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	v := p.Entry[0].Changes[0].Value
	if v.Message != "Test commment" ||
		v.CommentID != "122117665065405840_944595011715663" ||
		v.CreatedTime != 1792459906 {
		t.Fatalf("value: %+v", v)
	}
	if v.Post.ID != v.PostID || !v.Post.IsPublished ||
		v.Post.StatusType != "mobile_status_update" ||
		v.Post.PermalinkURL == "" {
		t.Fatalf("post: %+v", v.Post)
	}
}

func TestValidSignature(t *testing.T) {
	h := &Handler{AppSecret: testSecret}
	body := loadSample(t)
	if !h.validSignature(sign(body), body) {
		t.Error("valid signature rejected")
	}
	if h.validSignature(sign(body), append(body, ' ')) {
		t.Error("signature accepted for modified body")
	}
	if h.validSignature("sha1=abc", body) {
		t.Error("non-sha256 header accepted")
	}
}

func TestHandleEvent(t *testing.T) {
	body := loadSample(t)
	// Pool is nil: none of these cases may reach the database.
	h := &Handler{AppSecret: testSecret, TriggerKeyword: "order"}

	cases := []struct {
		name string
		sig  string
		want int
	}{
		{"bad signature", "sha256=00", http.StatusUnauthorized},
		{"comment without message is acked", sign(body), http.StatusOK},
		{"non-page object is acked", "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := body
			if tc.sig == "" {
				body = []byte(`{"object":"instagram","entry":[{"id":"1","changes":[{"field":"feed","value":{"item":"comment","verb":"add","message":"order"}}]}]}`)
				tc.sig = sign(body)
			}
			req := httptest.NewRequest(http.MethodPost, "/webhook/facebook", bytes.NewReader(body))
			req.Header.Set("X-Hub-Signature-256", tc.sig)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

func TestProcessCommentSkipsWithoutDB(t *testing.T) {
	created := time.Unix(1792459906, 0)
	h := &Handler{TriggerKeyword: "order", Now: func() time.Time { return created.Add(time.Hour) }}
	const page = "1267866303069580"

	cases := []struct {
		name string
		c    commentValue
	}{
		{"edit", commentValue{Verb: "edited", Message: "order"}},
		{"from the page itself", func() commentValue {
			c := commentValue{Verb: "add", Message: "order"}
			c.From.ID = page
			return c
		}()},
		{"no message", commentValue{Verb: "add"}},
		{"unpublished post", commentValue{Verb: "add", Message: "ORDER please",
			Post: postInfo{ID: "1267866303069580_122117665065405840"}}},
		{"older than 7 days", commentValue{Verb: "add", Message: "order",
			CreatedTime: created.Add(-8 * 24 * time.Hour).Unix()}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A nil Pool would panic if the comment got past the filters.
			if h.processComment(context.Background(), page, tc.c) {
				t.Error("unexpected retry")
			}
		})
	}
}

func TestMatchesKeyword(t *testing.T) {
	cases := []struct {
		message, keyword string
		want             bool
	}{
		{"I want to ORDER 2", "order", true},
		{"Test commment", "order", false},
		{"size M please", "Size", true},
		{"anything", "", false}, // no keyword configured must never match everything
	}
	for _, tc := range cases {
		if got := matchesKeyword(tc.message, tc.keyword); got != tc.want {
			t.Errorf("matchesKeyword(%q, %q) = %v, want %v", tc.message, tc.keyword, got, tc.want)
		}
	}
}
