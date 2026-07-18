package messenger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const graphBaseURL = "https://graph.facebook.com/v21.0"

type Client struct {
	PageAccessToken string
	HTTPClient      *http.Client
}

func New(pageAccessToken string) *Client {
	return &Client{PageAccessToken: pageAccessToken, HTTPClient: http.DefaultClient}
}

// SendPrivateReply sends the FIRST message to a user based on a comment they left.
// Only usable once per comment_id, and only within 7 days of the comment. This is
// what opens the Messenger thread — after this, you have a normal PSID to message.
func (c *Client) SendPrivateReply(commentID, text string) (psid string, err error) {
	body := map[string]any{
		"recipient": map[string]string{"comment_id": commentID},
		"message":   map[string]string{"text": text},
	}
	resp, err := c.post("/me/messages", body)
	if err != nil {
		return "", err
	}
	// Response contains the recipient_id (PSID) once the reply is delivered.
	var out struct {
		RecipientID string `json:"recipient_id"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return "", err
	}
	return out.RecipientID, nil
}

// SendText sends a plain text message to an already-known PSID (used once we're
// inside the messaging window, e.g. order confirmation).
func (c *Client) SendText(psid, text string) error {
	body := map[string]any{
		"recipient": map[string]string{"id": psid},
		"message":   map[string]string{"text": text},
	}
	_, err := c.post("/me/messages", body)
	return err
}

// SendProductCarousel sends a Generic Template with a button that deep-links to
// the order form webview for each product.
func (c *Client) SendProductCarousel(psid string, products []CarouselItem) error {
	elements := make([]map[string]any, 0, len(products))
	for _, p := range products {
		elements = append(elements, map[string]any{
			"title":     p.Title,
			"subtitle":  p.Subtitle,
			"image_url": p.ImageURL,
			"buttons": []map[string]string{
				{
					"type": "web_url",
					"url":  p.OrderFormURL,
					"title": "Order this",
				},
			},
		})
	}
	body := map[string]any{
		"recipient": map[string]string{"id": psid},
		"message": map[string]any{
			"attachment": map[string]any{
				"type": "template",
				"payload": map[string]any{
					"template_type": "generic",
					"elements":      elements,
				},
			},
		},
	}
	_, err := c.post("/me/messages", body)
	return err
}

type CarouselItem struct {
	Title        string
	Subtitle     string
	ImageURL     string
	OrderFormURL string
}

func (c *Client) post(path string, body any) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := graphBaseURL + path + "?access_token=" + c.PageAccessToken
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("messenger API error (%d): %s", resp.StatusCode, respBody)
	}
	return respBody, nil
}
