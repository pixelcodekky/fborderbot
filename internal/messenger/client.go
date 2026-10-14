package messenger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const graphBaseURL = "https://graph.facebook.com/v21.0"

type Client struct {
	PageAccessToken string
	HTTPClient      *http.Client
}

func New(pageAccessToken string) *Client {
	return &Client{PageAccessToken: pageAccessToken, HTTPClient: &http.Client{Timeout: 15 * time.Second}}
}

// APIError is a non-2xx response from the Graph API.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("messenger API error (%d): %s", e.StatusCode, e.Body)
}

// IsPermanent reports whether err is a Graph API rejection that will fail the
// same way on retry, e.g. an expired token or a comment older than 7 days.
func IsPermanent(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.StatusCode >= 400 && ae.StatusCode < 500 && ae.StatusCode != http.StatusTooManyRequests
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
	endpoint := graphBaseURL + path + "?access_token=" + c.PageAccessToken
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		// *url.Error embeds the full URL, access token included; keep it out of logs.
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("messenger %s %s: %w", ue.Op, path, ue.Err)
		}
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}
	return respBody, nil
}
