package web

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/erazumov/wgserver/internal/config"
	"github.com/erazumov/wgserver/internal/store"
)

func newTestAppWithDB(t *testing.T) *App {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "wgserver.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tmpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}
	return &App{
		DB:         db,
		Sessions:   NewSessionStore(time.Hour),
		CSRFKey:    []byte("test-csrf-key-32-bytes-long!!!"),
		Config:     &config.Config{Clients: config.ClientsConfig{CIDR: "10.0.1.0/24", Address: "10.0.1.1/24", Endpoint: "vpn.example.com:51821", PublicKey: "WG1_PUB_BASE64", DNSServers: []string{"1.1.1.1"}}},
		Templates:  tmpl,
		CookieName: "wgserver_session",
		WGRunner:   newFakeRunner(),
	}
}

func httpRouterForTest(app *App) http.Handler {
	return app.NewRouter()
}

func formReq(method, target string, values url.Values) *http.Request {
	r, err := http.NewRequest(method, target, io.NopCloser(strings.NewReader(values.Encode())))
	if err != nil {
		panic(err)
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func seedAdmin(t *testing.T, app *App, username, password string) int64 {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	id, err := store.CreateAdmin(context.Background(), app.DB, store.Admin{
		Username:     username,
		PasswordHash: string(hash),
		CreatedAt:    time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	return id
}

func TestHealthz(t *testing.T) {
	app := newTestAppWithDB(t)
	req := newReq(http.MethodGet, "/healthz")
	rec := httptest.NewRecorder()
	app.Healthz(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Errorf("body = %q, want ok", rec.Body.String())
	}
}

func TestLoginGet_RendersForm(t *testing.T) {
	app := newTestAppWithDB(t)
	rec := httptest.NewRecorder()
	app.LoginGet(rec, newReq(http.MethodGet, "/admin/login"))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `action="/admin/login"`) {
		t.Errorf("missing login form action\n%s", rec.Body.String())
	}
}

func TestLoginPost_SuccessSetsSessionAndRedirects(t *testing.T) {
	app := newTestAppWithDB(t)
	seedAdmin(t, app, "root", "s3cret")

	form := url.Values{}
	form.Set("username", "root")
	form.Set("password", "s3cret")
	req := formReq(http.MethodPost, "/admin/login", form)

	rec := httptest.NewRecorder()
	app.LoginPost(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want 302\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/" {
		t.Errorf("Location = %q, want /admin/", loc)
	}
	var found bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == app.CookieName && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Errorf("no session cookie set")
	}
}

func TestLoginPost_BadPasswordShowsError(t *testing.T) {
	app := newTestAppWithDB(t)
	seedAdmin(t, app, "root", "s3cret")

	form := url.Values{}
	form.Set("username", "root")
	form.Set("password", "wrong")
	req := formReq(http.MethodPost, "/admin/login", form)
	rec := httptest.NewRecorder()
	app.LoginPost(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (re-render with error)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid credentials") {
		t.Errorf("body missing 'invalid credentials'\n%s", rec.Body.String())
	}
}

func TestLoginPost_UnknownUserShowsError(t *testing.T) {
	app := newTestAppWithDB(t)
	form := url.Values{}
	form.Set("username", "ghost")
	form.Set("password", "x")
	req := formReq(http.MethodPost, "/admin/login", form)
	rec := httptest.NewRecorder()
	app.LoginPost(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid credentials") {
		t.Errorf("body missing 'invalid credentials'\n%s", rec.Body.String())
	}
}

func TestLogoutPost_ClearsSession(t *testing.T) {
	app := newTestAppWithDB(t)
	s, _ := app.Sessions.Create(1, "u")

	req := newReq(http.MethodPost, "/admin/logout")
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: s.ID})
	ctx := context.WithValue(req.Context(), sessionCtxKey, s)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	app.LogoutPost(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/login" {
		t.Errorf("Location = %q, want /admin/login", loc)
	}
	if _, ok := app.Sessions.Get(s.ID); ok {
		t.Error("session not destroyed")
	}
}

func TestDashboard_RendersEmpty(t *testing.T) {
	app := newTestAppWithDB(t)
	s, _ := app.Sessions.Create(1, "u")
	req := newReq(http.MethodGet, "/admin/")
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: s.ID})
	ctx := context.WithValue(req.Context(), sessionCtxKey, s)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	app.Dashboard(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no peers yet") {
		t.Errorf("missing 'no peers yet'\n%s", rec.Body.String())
	}
}

func TestDashboard_ListsPeerWithState(t *testing.T) {
	app := newTestAppWithDB(t)
	s, _ := app.Sessions.Create(1, "u")
	_, err := store.CreatePeer(context.Background(), app.DB, store.Peer{
		Name: "alice", PublicKey: "PK", PrivateKey: "PVT",
		AssignedIP: "10.0.1.2/32", CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}

	req := newReq(http.MethodGet, "/admin/")
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: s.ID})
	ctx := context.WithValue(req.Context(), sessionCtxKey, s)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	app.Dashboard(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "alice") {
		t.Errorf("missing peer name\n%s", body)
	}
	if !strings.Contains(body, "10.0.1.2/32") {
		t.Errorf("missing peer ip\n%s", body)
	}
	if !strings.Contains(body, "pending sync") {
		t.Errorf("missing pending-sync badge\n%s", body)
	}
}

func TestPeerNewGet_RendersForm(t *testing.T) {
	app := newTestAppWithDB(t)
	s, _ := app.Sessions.Create(1, "u")
	req := newReq(http.MethodGet, "/admin/peers/new")
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: s.ID})
	ctx := context.WithValue(req.Context(), sessionCtxKey, s)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	app.PeerNewGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `name="name"`) {
		t.Errorf("missing name field")
	}
}

func TestPeerNewPost_CreatesPeerAndRedirects(t *testing.T) {
	app := newTestAppWithDB(t)
	adminID := seedAdmin(t, app, "root", "x")
	s, _ := app.Sessions.Create(adminID, "root")
	form := url.Values{}
	form.Set("name", "alice-laptop")
	form.Set("generate_psk", "on")
	req := formReq(http.MethodPost, "/admin/peers", form)
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: s.ID})
	ctx := context.WithValue(req.Context(), sessionCtxKey, s)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	app.PeerNewPost(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want 302\n%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/admin/" {
		t.Errorf("Location = %q, want /admin/", loc)
	}

	pending, err := store.ListPeersPendingSync(context.Background(), app.DB)
	if err != nil {
		t.Fatalf("ListPeersPendingSync: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending peers = %d, want 1", len(pending))
	}
	if pending[0].Name != "alice-laptop" {
		t.Errorf("Name = %q, want alice-laptop", pending[0].Name)
	}
	if pending[0].PrivateKey == "" {
		t.Error("PrivateKey not set")
	}
	if pending[0].PublicKey == "" {
		t.Error("PublicKey not set")
	}
	if pending[0].AssignedIP != "10.0.1.2/32" {
		t.Errorf("AssignedIP = %q, want 10.0.1.2/32", pending[0].AssignedIP)
	}
	// Default form has generate_psk=on (checked), so PSK is set.
	if pending[0].PresharedKey == nil {
		t.Error("PresharedKey not set despite generate_psk=on in form")
	} else if *pending[0].PresharedKey != "FAKE_PSK_BASE64=" {
		t.Errorf("PresharedKey = %q, want FAKE_PSK_BASE64=", *pending[0].PresharedKey)
	}
}

func TestPeerNewPost_GeneratePSKOffStoresNilPSK(t *testing.T) {
	app := newTestAppWithDB(t)
	adminID := seedAdmin(t, app, "root", "x")
	s, _ := app.Sessions.Create(adminID, "root")
	form := url.Values{}
	form.Set("name", "no-psk-peer")
	// checkbox absent → no PSK
	req := formReq(http.MethodPost, "/admin/peers", form)
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: s.ID})
	ctx := context.WithValue(req.Context(), sessionCtxKey, s)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	app.PeerNewPost(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}

	pending, _ := store.ListPeersPendingSync(context.Background(), app.DB)
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	if pending[0].PresharedKey != nil {
		t.Errorf("PresharedKey = %q, want nil (checkbox off)", *pending[0].PresharedKey)
	}
}

func TestPeerNewPost_EmptyName400(t *testing.T) {
	app := newTestAppWithDB(t)
	s, _ := app.Sessions.Create(1, "u")
	form := url.Values{}
	form.Set("name", "")
	req := formReq(http.MethodPost, "/admin/peers", form)
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: s.ID})
	ctx := context.WithValue(req.Context(), sessionCtxKey, s)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	app.PeerNewPost(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestPeerNewPost_HTMXEmptyName_RerendersFormWithError(t *testing.T) {
	app := newTestAppWithDB(t)
	s, _ := app.Sessions.Create(1, "u")
	form := url.Values{}
	form.Set("name", "")
	req := formReq(http.MethodPost, "/admin/peers", form)
	req.Header.Set("HX-Request", "true")
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: s.ID})
	ctx := context.WithValue(req.Context(), sessionCtxKey, s)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	app.PeerNewPost(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (form re-render for HTMX)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "name required") {
		t.Errorf("body missing 'name required' error\n%s", body)
	}
	// Should be a fragment, not a full layout.
	if strings.Contains(body, "<!doctype html>") {
		t.Errorf("HTMX response unexpectedly contains full layout")
	}
	if !strings.Contains(body, `hx-post="/admin/peers"`) {
		t.Errorf("body missing hx-post attribute (form fragment)\n%s", body)
	}
}

func TestPeerNewPost_HTMXSuccess_SetsHXRedirect(t *testing.T) {
	app := newTestAppWithDB(t)
	adminID := seedAdmin(t, app, "root", "x")
	s, _ := app.Sessions.Create(adminID, "root")
	form := url.Values{}
	form.Set("name", "alice")
	req := formReq(http.MethodPost, "/admin/peers", form)
	req.Header.Set("HX-Request", "true")
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: s.ID})
	ctx := context.WithValue(req.Context(), sessionCtxKey, s)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	app.PeerNewPost(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if hx := rec.Header().Get("HX-Redirect"); hx != "/admin/" {
		t.Errorf("HX-Redirect = %q, want /admin/", hx)
	}
}

func TestPeerRevokePost_DisablesPeer(t *testing.T) {
	app := newTestAppWithDB(t)
	s, _ := app.Sessions.Create(1, "u")
	_, err := store.CreatePeer(context.Background(), app.DB, store.Peer{
		Name: "p", PublicKey: "K", PrivateKey: "PV",
		AssignedIP: "10.0.1.2/32", CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}

	router := httpRouterForTest(app)
	req := newReq(http.MethodPost, "/admin/peers/1/revoke")
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: s.ID})
	ctx := context.WithValue(req.Context(), sessionCtxKey, s)
	req = req.WithContext(ctx)
	req.Header.Set("X-CSRF-Token", SignCSRF(app.CSRFKey, s.ID))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want 302\n%s", rec.Code, rec.Body.String())
	}

	got, err := store.GetPeerByID(context.Background(), app.DB, 1)
	if err != nil {
		t.Fatalf("GetPeerByID: %v", err)
	}
	if !got.Disabled {
		t.Error("peer not disabled")
	}
	if !got.PendingSync {
		t.Error("after DisablePeer: pending_sync should be 1 again (WG must observe the removal)")
	}
}

func TestPeerConfigGet_ReturnsConfFile(t *testing.T) {
	app := newTestAppWithDB(t)
	s, _ := app.Sessions.Create(1, "u")
	_, err := store.CreatePeer(context.Background(), app.DB, store.Peer{
		Name: "alice-laptop", PublicKey: "PUB", PrivateKey: "PRIV",
		AssignedIP: "10.0.1.2/32", CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}

	router := httpRouterForTest(app)
	req := newReq(http.MethodGet, "/admin/peers/1/config")
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: s.ID})
	ctx := context.WithValue(req.Context(), sessionCtxKey, s)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"PrivateKey = PRIV",
		"Address = 10.0.1.2/32",
		"PublicKey = WG1_PUB_BASE64",
		"Endpoint = vpn.example.com:51821",
		"DNS = 1.1.1.1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("conf body missing %q\n---\n%s\n---", want, body)
		}
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".conf") {
		t.Errorf("Content-Disposition = %q, want attachment with .conf", cd)
	}
}

var _ = bytes.NewBuffer
