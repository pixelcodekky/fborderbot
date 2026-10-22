package messenger

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendPrivateReplyButtons(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/me/messages" || r.URL.Query().Get("access_token") != "tok" {
			t.Errorf("request to %s", r.URL)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Error(err)
		}
		w.Write([]byte(`{"recipient_id":"psid-1","message_id":"m1"}`))
	}))
	defer srv.Close()

	c := &Client{PageAccessToken: "tok", HTTPClient: srv.Client(), BaseURL: srv.URL}
	psid, err := c.SendPrivateReplyButtons("c1", "Order: x\nTotal: SGD 1\nChoose Payment", []Button{
		{Type: "postback", Title: "PayNow", Payload: "PAY:1:paynow"},
	})
	if err != nil || psid != "psid-1" {
		t.Fatalf("psid=%q err=%v", psid, err)
	}

	want := `{"message":{"attachment":{"payload":{"buttons":[{"payload":"PAY:1:paynow","title":"PayNow","type":"postback"}],` +
		`"template_type":"button","text":"Order: x\nTotal: SGD 1\nChoose Payment"},"type":"template"}},"recipient":{"comment_id":"c1"}}`
	gotJSON, _ := json.Marshal(got)
	if string(gotJSON) != want {
		t.Errorf("body\n got  %s\n want %s", gotJSON, want)
	}
}
