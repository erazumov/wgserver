package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Sender is the subset of the Telegram Bot API the bot needs. It is
// implemented by httpSender in production and by hand-rolled fakes in
// tests (see bot_test.go).
type Sender interface {
	GetUpdates(ctx context.Context, offset, timeout int) ([]Update, error)
	SendMessage(ctx context.Context, chatID int64, text string) error
	SendDocument(ctx context.Context, chatID int64, filename string, body []byte, caption string) error
}

// apiResponse is the envelope every Telegram Bot API call returns.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result,omitempty"`
	ErrorCode   int             `json:"error_code,omitempty"`
	Description string          `json:"description,omitempty"`
}

type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
}

type Message struct {
	MessageID int64  `json:"message_id"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
	From      *User  `json:"from,omitempty"`
	Chat      *Chat  `json:"chat,omitempty"`
}

type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type Chat struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

// HTTPSender calls the Bot API. baseURL is the api.telegram.org root
// (overridable in tests via httptest).
type HTTPSender struct {
	token   string
	baseURL string
	client  *http.Client
}

func NewHTTPSender(token, baseURL string) *HTTPSender {
	if baseURL == "" {
		baseURL = "https://api.telegram.org"
	}
	return &HTTPSender{
		token:   token,
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (s *HTTPSender) endpoint(method string) string {
	return s.baseURL + "/bot" + s.token + "/" + method
}

func (s *HTTPSender) GetUpdates(ctx context.Context, offset, timeoutSec int) ([]Update, error) {
	q := url.Values{}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	q.Set("timeout", strconv.Itoa(timeoutSec))
	q.Set("allowed_updates", `["message"]`)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint("getUpdates")+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("getUpdates: new request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getUpdates: %w", err)
	}
	defer resp.Body.Close()

	var env apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("getUpdates: decode: %w", err)
	}
	if !env.OK {
		return nil, fmt.Errorf("getUpdates: api error %d: %s", env.ErrorCode, env.Description)
	}
	var updates []Update
	if err := json.Unmarshal(env.Result, &updates); err != nil {
		return nil, fmt.Errorf("getUpdates: parse result: %w", err)
	}
	return updates, nil
}

func (s *HTTPSender) SendMessage(ctx context.Context, chatID int64, text string) error {
	payload, _ := json.Marshal(map[string]any{
		"chat_id": chatID,
		"text":    text,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint("sendMessage"), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("sendMessage: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return s.doSimple(req, "sendMessage")
}

func (s *HTTPSender) SendDocument(ctx context.Context, chatID int64, filename string, body []byte, caption string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return fmt.Errorf("sendDocument: write chat_id: %w", err)
	}
	if caption != "" {
		if err := w.WriteField("caption", caption); err != nil {
			return fmt.Errorf("sendDocument: write caption: %w", err)
		}
	}
	fw, err := w.CreateFormFile("document", filename)
	if err != nil {
		return fmt.Errorf("sendDocument: create form file: %w", err)
	}
	if _, err := fw.Write(body); err != nil {
		return fmt.Errorf("sendDocument: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("sendDocument: close multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint("sendDocument"), &buf)
	if err != nil {
		return fmt.Errorf("sendDocument: new request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return s.doSimple(req, "sendDocument")
}

func (s *HTTPSender) doSimple(req *http.Request, op string) error {
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s: read body: %w", op, err)
	}
	var env apiResponse
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("%s: decode: %w (body=%q)", op, err, string(body))
	}
	if !env.OK {
		return fmt.Errorf("%s: api error %d: %s", op, env.ErrorCode, env.Description)
	}
	return nil
}
