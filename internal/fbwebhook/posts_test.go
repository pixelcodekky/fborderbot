package fbwebhook

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestProcessPostSkipsWithoutDB(t *testing.T) {
	h := &Handler{TriggerKeyword: "order"}
	const page = "1267866303069580"

	post := func(verb, message, from string) commentValue {
		p := commentValue{Item: "status", Verb: verb, Message: message,
			PostID: page + "_122117665065405840"}
		p.From.ID = from
		return p
	}
	cases := []struct {
		name string
		p    commentValue
	}{
		{"visitor post", post("add", "comment ORDER to buy", "28768664042804007")},
		{"no text (photo only)", post("add", "", page)},
		{"missing post_id", commentValue{Item: "photo", Verb: "add", Message: "order"}},
		{"other verb", post("hide", "order", page)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A nil Pool would panic if the post got past the filters.
			if h.processPost(context.Background(), page, tc.p) {
				t.Error("unexpected retry")
			}
		})
	}
}

func TestHandleEventRoutesPosts(t *testing.T) {
	h := &Handler{AppSecret: testSecret, TriggerKeyword: "order"}
	// A new Page post without text must be acked without touching the DB;
	// a reaction (item "reaction") must be ignored entirely.
	body := []byte(`{"object":"page","entry":[{"id":"1267866303069580","changes":[
		{"field":"feed","value":{"item":"photo","verb":"add","post_id":"1267866303069580_1","from":{"id":"1267866303069580"}}},
		{"field":"feed","value":{"item":"reaction","verb":"add","post_id":"1267866303069580_1","reaction_type":"like"}}]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/webhook/facebook", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestFindKeywords(t *testing.T) {
	keywords := []string{"buy", "order", "PM"}
	cases := []struct {
		message string
		want    []string
	}{
		{"New red dress! Comment ORDER or BUY to get it", []string{"buy", "order"}},
		{"pm for price", []string{"PM"}},
		{"New stock arriving soon!", nil},
		{"", nil},
	}
	for _, tc := range cases {
		got := findKeywords(tc.message, keywords)
		if !slices.Equal(got, tc.want) {
			t.Errorf("findKeywords(%q) = %v, want %v", tc.message, got, tc.want)
		}
	}
}
