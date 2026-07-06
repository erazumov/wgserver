package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHTTPSender_GetUpdates_DecodesOK(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": []map[string]any{
				{
					"update_id": 7,
					"message": map[string]any{
						"message_id": 11,
						"date":       1700000000,
						"text":       "/start",
						"from": map[string]any{
							"id":         42,
							"first_name": "Alice",
							"username":   "alice",
						},
						"chat": map[string]any{
							"id":    -1001,
							"title": "vpn",
							"type":  "supergroup",
						},
					},
				},
			},
		})
	}))
	defer srv.Close()

	s := NewHTTPSender("TOKEN", srv.URL)
	updates, err := s.GetUpdates(context.Background(), 5, 25)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(updates))
	}
	u := updates[0]
	if u.UpdateID != 7 {
		t.Errorf("UpdateID = %d, want 7", u.UpdateID)
	}
	if u.Message == nil {
		t.Fatal("Message is nil")
	}
	if u.Message.Text != "/start" {
		t.Errorf("Text = %q, want /start", u.Message.Text)
	}
	if u.Message.From == nil || u.Message.From.ID != 42 {
		t.Errorf("From.ID = %v, want 42", u.Message.From)
	}
	if u.Message.Chat == nil || u.Message.Chat.ID != -1001 {
		t.Errorf("Chat.ID = %v, want -1001", u.Message.Chat)
	}
	if gotQuery.Get("offset") != "5" {
		t.Errorf("offset = %q, want 5", gotQuery.Get("offset"))
	}
	if gotQuery.Get("timeout") != "25" {
		t.Errorf("timeout = %q, want 25", gotQuery.Get("timeout"))
	}
}

func TestHTTPSender_GetUpdates_NotOKReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":          false,
			"error_code":  401,
			"description": "unauthorized",
		})
	}))
	defer srv.Close()

	s := NewHTTPSender("BAD", srv.URL)
	_, err := s.GetUpdates(context.Background(), 0, 1)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("err = %v, want it to contain 'unauthorized'", err)
	}
}

func TestHTTPSender_SendMessage_PostsJSON(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{}})
	}))
	defer srv.Close()

	s := NewHTTPSender("TOKEN", srv.URL)
	if err := s.SendMessage(context.Background(), 123, "hello"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if gotPath != "/botTOKEN/sendMessage" {
		t.Errorf("path = %q, want /botTOKEN/sendMessage", gotPath)
	}
	if gotBody["chat_id"].(float64) != 123 {
		t.Errorf("chat_id = %v, want 123", gotBody["chat_id"])
	}
	if gotBody["text"] != "hello" {
		t.Errorf("text = %v, want hello", gotBody["text"])
	}
}

func TestHTTPSender_SendDocument_PostsMultipart(t *testing.T) {
	var gotPath string
	var gotContentType string
	var gotChatID string
	var gotCaption string
	var gotFilename string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("MultipartReader: %v", err)
			return
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("NextPart: %v", err)
				return
			}
			b, _ := io.ReadAll(part)
			switch part.FormName() {
			case "chat_id":
				gotChatID = string(b)
			case "caption":
				gotCaption = string(b)
			case "document":
				gotFilename = part.FileName()
				gotBody = b
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{}})
	}))
	defer srv.Close()

	s := NewHTTPSender("TOKEN", srv.URL)
	body := []byte("private-key=ABC\n")
	if err := s.SendDocument(context.Background(), 42, "alice.conf", body, "your config"); err != nil {
		t.Fatalf("SendDocument: %v", err)
	}
	if gotPath != "/botTOKEN/sendDocument" {
		t.Errorf("path = %q, want /botTOKEN/sendDocument", gotPath)
	}
	if !strings.HasPrefix(gotContentType, "multipart/form-data; boundary=") {
		t.Errorf("Content-Type = %q, want multipart/form-data", gotContentType)
	}
	if gotChatID != "42" {
		t.Errorf("chat_id = %q, want 42", gotChatID)
	}
	if gotCaption != "your config" {
		t.Errorf("caption = %q, want 'your config'", gotCaption)
	}
	if gotFilename != "alice.conf" {
		t.Errorf("filename = %q, want alice.conf", gotFilename)
	}
	if string(gotBody) != string(body) {
		t.Errorf("document body mismatch: got %q, want %q", gotBody, body)
	}
}

func TestHTTPSender_SendMessage_PropagatesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": false, "error_code": 400, "description": "chat not found",
		})
	}))
	defer srv.Close()

	s := NewHTTPSender("TOKEN", srv.URL)
	err := s.SendMessage(context.Background(), 999, "x")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("err = %v, want it to contain 'chat not found'", err)
	}
}
