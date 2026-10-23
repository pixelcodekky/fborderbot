package twoc2pwebhook

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"fb-order-bot/internal/twoc2p"
)

func TestRejectsUnsignedNotification(t *testing.T) {
	// Nil Pool: a bad signature must be rejected before any database access.
	h := &Handler{Client: twoc2p.New("", "M1", "secret", "", "")}
	for _, body := range []string{`{}`, `not json`, `{"payload":"a.b.c"}`} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhooks/2c2p", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status %d", body, rec.Code)
		}
	}
}
