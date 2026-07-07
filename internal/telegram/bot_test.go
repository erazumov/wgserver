package telegram

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erazumov/wgserver/internal/store"
)

type fakeSender struct {
	mu           sync.Mutex
	getUpdatesCh chan []Update
	getUpdatesFn func(offset, timeout int) ([]Update, error)

	sentMsgs    []sentMessage
	sentDocs    []sentDocument
	failSendMsg error

	// Self-test stubs. Tests can pre-set these; if not set, the
	// defaults are "healthy" (getMe returns a plausible bot, getChat
	// returns a chat whose id matches whatever the test bot has).
	me     *BotInfo
	meErr  error
	chat   *Chat
	chatID int64
	chErr  error
}

type sentMessage struct {
	ChatID int64
	Text   string
}

type sentDocument struct {
	ChatID   int64
	Filename string
	Caption  string
	Body     []byte
}

func newFakeSender() *fakeSender {
	return &fakeSender{getUpdatesCh: make(chan []Update, 4)}
}

func (f *fakeSender) GetUpdates(ctx context.Context, offset, timeout int) ([]Update, error) {
	f.mu.Lock()
	fn := f.getUpdatesFn
	ch := f.getUpdatesCh
	f.mu.Unlock()
	if fn != nil {
		return fn(offset, timeout)
	}
	select {
	case us := <-ch:
		return us, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(2 * time.Second):
		return nil, nil
	}
}

func (f *fakeSender) SendMessage(ctx context.Context, chatID int64, text string) error {
	if f.failSendMsg != nil {
		return f.failSendMsg
	}
	f.mu.Lock()
	f.sentMsgs = append(f.sentMsgs, sentMessage{chatID, text})
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) SendDocument(ctx context.Context, chatID int64, filename string, body []byte, caption string) error {
	f.mu.Lock()
	f.sentDocs = append(f.sentDocs, sentDocument{chatID, filename, caption, append([]byte(nil), body...)})
	f.mu.Unlock()
	return nil
}

func (f *fakeSender) sentMsgsCopy() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentMessage, len(f.sentMsgs))
	copy(out, f.sentMsgs)
	return out
}

func (f *fakeSender) sentDocsCopy() []sentDocument {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentDocument, len(f.sentDocs))
	copy(out, f.sentDocs)
	return out
}

func (f *fakeSender) GetMe(_ context.Context) (*BotInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.meErr != nil {
		return nil, f.meErr
	}
	if f.me != nil {
		return f.me, nil
	}
	return &BotInfo{ID: 1, Username: "testbot", FirstName: "Test", IsBot: true}, nil
}

func (f *fakeSender) GetChat(_ context.Context, chatID int64) (*Chat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chErr != nil {
		return nil, f.chErr
	}
	if f.chat != nil {
		return f.chat, nil
	}
	return &Chat{ID: chatID, Title: "test chat", Type: "supergroup"}, nil
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "tgbot.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newTestBot(t *testing.T) (*Bot, *fakeSender) {
	t.Helper()
	db := openTestDB(t)
	fs := newFakeSender()
	bot := &Bot{
		DB:              db,
		Sender:          fs,
		GenKeyPair:      func() (string, string, error) { return "FAKE_PRIV", "FAKE_PUB", nil },
		GroupChatID:     -1001,
		PerUserQuota:    2,
		Logger:          log.New(io.Discard, "", 0),
		ServerPublicKey: "WG1_PUB_BASE64",
		ServerEndpoint:  "taigaproxy.example.com:51821",
		DNSServers:      []string{"1.1.1.1", "9.9.9.9"},
		CIDR:            "10.0.1.0/24",
		ServerAddr:      "10.0.1.1/24",
	}
	return bot, fs
}

func startMessage(userID int64, username, firstName, text string) Update {
	return Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 100,
			Date:      time.Now().Unix(),
			Text:      text,
			From:      &User{ID: userID, Username: username, FirstName: firstName},
			Chat:      &Chat{ID: -1001, Title: "vpn", Type: "supergroup"},
		},
	}
}

func otherChatMessage(userID int64, text string) Update {
	u := startMessage(userID, "x", "X", text)
	u.Message.Chat.ID = -9999
	return u
}

func TestHandleUpdate_IgnoresNilMessage(t *testing.T) {
	bot, fs := newTestBot(t)
	if err := bot.handleUpdate(context.Background(), Update{UpdateID: 1}); err != nil {
		t.Fatalf("handleUpdate: %v", err)
	}
	if len(fs.sentMsgsCopy()) != 0 || len(fs.sentDocsCopy()) != 0 {
		t.Error("no actions expected on nil message")
	}
}

func TestHandleUpdate_IgnoresNonStartText(t *testing.T) {
	bot, fs := newTestBot(t)
	if err := bot.handleUpdate(context.Background(), startMessage(42, "alice", "Alice", "hello")); err != nil {
		t.Fatalf("handleUpdate: %v", err)
	}
	if len(fs.sentMsgsCopy()) != 0 || len(fs.sentDocsCopy()) != 0 {
		t.Error("non-/start text must not trigger claim or reply")
	}
}

func TestHandleUpdate_IgnoresOtherGroups(t *testing.T) {
	bot, fs := newTestBot(t)
	if err := bot.handleUpdate(context.Background(), otherChatMessage(42, "/start")); err != nil {
		t.Fatalf("handleUpdate: %v", err)
	}
	if len(fs.sentMsgsCopy()) != 0 || len(fs.sentDocsCopy()) != 0 {
		t.Error("message from foreign chat must be ignored")
	}
}

func TestHandleUpdate_StartInGroup_ClaimsAndSendsConf(t *testing.T) {
	bot, fs := newTestBot(t)
	ctx := context.Background()

	if err := bot.handleUpdate(ctx, startMessage(42, "alice", "Alice", "/start")); err != nil {
		t.Fatalf("handleUpdate: %v", err)
	}

	docs := fs.sentDocsCopy()
	if len(docs) != 1 {
		t.Fatalf("sentDocs = %d, want 1", len(docs))
	}
	d := docs[0]
	if d.ChatID != 42 {
		t.Errorf("document ChatID = %d, want 42 (DM to user, not to group)", d.ChatID)
	}
	if d.Filename != "tg-42.conf" {
		t.Errorf("filename = %q, want tg-42.conf", d.Filename)
	}
	body := string(d.Body)
	for _, want := range []string{
		"PrivateKey = FAKE_PRIV",
		"Address = 10.0.1.2/32",
		"PublicKey = WG1_PUB_BASE64",
		"Endpoint = taigaproxy.example.com:51821",
		"DNS = 1.1.1.1, 9.9.9.9",
		"AllowedIPs = 0.0.0.0/0, ::/0",
		"PersistentKeepalive = 25",
	} {
		if !contains(body, want) {
			t.Errorf("conf body missing %q\n---\n%s\n---", want, body)
		}
	}
	if len(fs.sentMsgsCopy()) != 0 {
		t.Errorf("no message reply expected on successful claim, got %d", len(fs.sentMsgsCopy()))
	}

	pending, err := store.ListPeersPendingSync(ctx, bot.DB)
	if err != nil {
		t.Fatalf("ListPeersPendingSync: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending peers = %d, want 1", len(pending))
	}
	if pending[0].CreatedByTelegramUserID == nil || *pending[0].CreatedByTelegramUserID != 42 {
		t.Errorf("CreatedByTelegramUserID = %v, want 42", pending[0].CreatedByTelegramUserID)
	}
	if pending[0].CreatedByAdminID != nil {
		t.Errorf("CreatedByAdminID = %v, want nil (must not be set)", pending[0].CreatedByAdminID)
	}
}

func TestHandleUpdate_StartInGroup_QuotaExceeded_Replies(t *testing.T) {
	bot, fs := newTestBot(t)
	bot.PerUserQuota = 1
	ctx := context.Background()

	if err := bot.handleUpdate(ctx, startMessage(7, "bob", "Bob", "/start")); err != nil {
		t.Fatalf("first handleUpdate: %v", err)
	}
	if len(fs.sentDocsCopy()) != 1 {
		t.Fatalf("first claim: docs = %d, want 1", len(fs.sentDocsCopy()))
	}

	if err := bot.handleUpdate(ctx, startMessage(7, "bob", "Bob", "/start")); err != nil {
		t.Fatalf("second handleUpdate: %v", err)
	}
	msgs := fs.sentMsgsCopy()
	if len(msgs) != 1 {
		t.Fatalf("msgs = %d, want 1 (quota reply)", len(msgs))
	}
	if msgs[0].ChatID != 7 {
		t.Errorf("quota reply ChatID = %d, want 7 (DM to user)", msgs[0].ChatID)
	}
	if !contains(msgs[0].Text, "quota") {
		t.Errorf("quota reply text = %q, want it to mention 'quota'", msgs[0].Text)
	}
	if len(fs.sentDocsCopy()) != 1 {
		t.Errorf("second claim must not produce a .conf; docs = %d, want 1", len(fs.sentDocsCopy()))
	}
}

func TestHandleUpdate_KeyGenFails_RepliesError(t *testing.T) {
	bot, fs := newTestBot(t)
	bot.GenKeyPair = func() (string, string, error) {
		return "", "", errors.New("wg genkey not found")
	}
	if err := bot.handleUpdate(context.Background(), startMessage(11, "c", "C", "/start")); err != nil {
		t.Fatalf("handleUpdate: %v", err)
	}
	msgs := fs.sentMsgsCopy()
	if len(msgs) != 1 {
		t.Fatalf("msgs = %d, want 1 (error reply)", len(msgs))
	}
	if !contains(msgs[0].Text, "error") {
		t.Errorf("error reply = %q, want it to mention 'error'", msgs[0].Text)
	}
	if len(fs.sentDocsCopy()) != 0 {
		t.Errorf("no .conf expected on keygen failure")
	}
}

func TestRun_ExitsOnContextCancel(t *testing.T) {
	bot, _ := newTestBot(t)
	bot.PollTimeout = 1 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		bot.Run(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel")
	}
}

func TestRun_ProcessesIncomingUpdates(t *testing.T) {
	bot, fs := newTestBot(t)
	bot.PollTimeout = 1 * time.Second
	fs.getUpdatesCh <- []Update{startMessage(99, "u", "U", "/start")}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		bot.Run(ctx)
		close(done)
	}()

	<-done

	if len(fs.sentDocsCopy()) != 1 {
		t.Errorf("docs = %d, want 1", len(fs.sentDocsCopy()))
	}
}

// captureLogger returns a logger writing to the returned *bytes.Buffer
// via a discard-everything-else fallback. Used by the startupCheck
// tests so we can assert on what the bot logged.
func captureLogger() (*log.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return log.New(buf, "", 0), buf
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestStartupCheck_BothOK(t *testing.T) {
	bot, fs := newTestBot(t)
	bot.GroupChatID = -1001
	fs.me = &BotInfo{ID: 42, Username: "meshdrop_bot", FirstName: "connectme", IsBot: true}
	fs.chat = &Chat{ID: -1001, Title: "vpn-gateway", Type: "supergroup"}

	logger, buf := captureLogger()
	bot.Logger = logger
	bot.startupCheck(context.Background())

	out := buf.String()
	for _, want := range []string{
		"getMe OK",
		"@meshdrop_bot",
		"getChat OK",
		"vpn-gateway",
		"supergroup",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("startupCheck output missing %q\n---\n%s\n---", want, out)
		}
	}
	if strings.Contains(out, "FAILED") {
		t.Errorf("startupCheck reported FAILED but both calls succeeded:\n%s", out)
	}
}

func TestStartupCheck_GetMeFails_LogsButContinues(t *testing.T) {
	bot, fs := newTestBot(t)
	fs.meErr = errors.New("api error 401: Unauthorized")

	logger, buf := captureLogger()
	bot.Logger = logger
	bot.startupCheck(context.Background())

	out := buf.String()
	if !strings.Contains(out, "getMe FAILED") {
		t.Errorf("startupCheck must log getMe FAILED, got:\n%s", out)
	}
	if !strings.Contains(out, "401") {
		t.Errorf("startupCheck should include underlying error, got:\n%s", out)
	}
	// Must NOT panic, must continue to the getChat call. The
	// operator at least sees the token problem and the chat-miss
	// hint together.
	if !strings.Contains(out, "getChat") {
		t.Errorf("startupCheck should still attempt getChat after getMe fails, got:\n%s", out)
	}
}

func TestStartupCheck_GetChatFails_ListsCommonCauses(t *testing.T) {
	bot, fs := newTestBot(t)
	bot.GroupChatID = -1001
	fs.chErr = errors.New("api error 400: Bad Request: chat not found")

	logger, buf := captureLogger()
	bot.Logger = logger
	bot.startupCheck(context.Background())

	out := buf.String()
	for _, want := range []string{
		"getChat(-1001) FAILED",
		"chat not found",
		"most common 'bot ignores /start' cause",
		"not a member",
		"chat_id in wgserver.yaml is wrong",
		"kicked/blocked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("startupCheck output missing %q\n---\n%s\n---", want, out)
		}
	}
}

func TestStartupCheck_NoGroupConfigured_SkipsGetChat(t *testing.T) {
	bot, _ := newTestBot(t)
	bot.GroupChatID = 0 // operator disabled the bot

	logger, buf := captureLogger()
	bot.Logger = logger
	bot.startupCheck(context.Background())

	out := buf.String()
	if !strings.Contains(out, "no group_chat_id configured") {
		t.Errorf("startupCheck must explain why getChat was skipped, got:\n%s", out)
	}
	if strings.Contains(out, "getChat OK") {
		t.Errorf("getChat must not be called when group_chat_id is 0:\n%s", out)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
