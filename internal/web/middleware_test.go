package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestApp(t *testing.T) *App {
	t.Helper()
	return &App{
		Sessions:   NewSessionStore(time.Hour),
		CSRFKey:    []byte("test-csrf-key-32-bytes-long!!!"),
		CookieName: "wgserver_session",
	}
}

func newReq(method, target string) *http.Request {
	r, err := http.NewRequest(method, target, nil)
	if err != nil {
		panic(err)
	}
	return r
}

func TestLoadSession_AddsSessionToContext(t *testing.T) {
	app := newTestApp(t)
	s, _ := app.Sessions.Create(7, "alice")

	req := newReq(http.MethodGet, "/admin/")
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: s.ID})
	rec := httptest.NewRecorder()

	var got *Session
	app.LoadSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = SessionFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)

	if got == nil {
		t.Fatal("SessionFromContext: nil")
	}
	if got.ID != s.ID {
		t.Errorf("ID = %q, want %q", got.ID, s.ID)
	}
}

func TestLoadSession_NoCookieLeavesNil(t *testing.T) {
	app := newTestApp(t)
	req := newReq(http.MethodGet, "/admin/")
	rec := httptest.NewRecorder()
	var got *Session
	app.LoadSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = SessionFromContext(r.Context())
	})).ServeHTTP(rec, req)
	if got != nil {
		t.Errorf("SessionFromContext = %+v, want nil", got)
	}
}

func TestLoadSession_InvalidCookieLeavesNil(t *testing.T) {
	app := newTestApp(t)
	req := newReq(http.MethodGet, "/admin/")
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: "garbage"})
	rec := httptest.NewRecorder()
	var got *Session
	app.LoadSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = SessionFromContext(r.Context())
	})).ServeHTTP(rec, req)
	if got != nil {
		t.Errorf("SessionFromContext = %+v, want nil", got)
	}
}

func TestRequireAdmin_RedirectsWithoutSession(t *testing.T) {
	app := newTestApp(t)
	req := newReq(http.MethodGet, "/admin/")
	rec := httptest.NewRecorder()
	app.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler should not be called")
	})).ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("status = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "/admin/login" {
		t.Errorf("Location = %q, want /admin/login", loc)
	}
}

func TestRequireAdmin_AllowsWithSession(t *testing.T) {
	app := newTestApp(t)
	s, _ := app.Sessions.Create(7, "alice")
	req := newReq(http.MethodGet, "/admin/")
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: s.ID})
	rec := httptest.NewRecorder()
	called := false
	app.LoadSession(app.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))).ServeHTTP(rec, req)

	if !called {
		t.Error("inner handler not called")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestRequireCSRF_RejectsPOSTWithoutToken(t *testing.T) {
	app := newTestApp(t)
	s, _ := app.Sessions.Create(1, "u")
	req := newReq(http.MethodPost, "/admin/peers")
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: s.ID})
	rec := httptest.NewRecorder()

	app.LoadSession(app.RequireCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler should not be called")
	}))).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestRequireCSRF_RejectsBadToken(t *testing.T) {
	app := newTestApp(t)
	s, _ := app.Sessions.Create(1, "u")
	req := newReq(http.MethodPost, "/admin/peers")
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: s.ID})
	req.Header.Set("X-CSRF-Token", "definitely-wrong")
	rec := httptest.NewRecorder()

	app.LoadSession(app.RequireCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner handler should not be called")
	}))).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestRequireCSRF_AcceptsValidToken(t *testing.T) {
	app := newTestApp(t)
	s, _ := app.Sessions.Create(1, "u")
	tok := SignCSRF(app.CSRFKey, s.ID)

	req := newReq(http.MethodPost, "/admin/peers")
	req.AddCookie(&http.Cookie{Name: app.CookieName, Value: s.ID})
	req.Header.Set("X-CSRF-Token", tok)
	rec := httptest.NewRecorder()
	called := false
	app.LoadSession(app.RequireCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))).ServeHTTP(rec, req)

	if !called {
		t.Error("inner handler not called")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestRequireCSRF_GETDoesNotCheck(t *testing.T) {
	app := newTestApp(t)
	req := newReq(http.MethodGet, "/admin/")
	rec := httptest.NewRecorder()
	called := false
	app.LoadSession(app.RequireCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))).ServeHTTP(rec, req)

	if !called {
		t.Error("GET should pass through without CSRF check")
	}
}

func TestSessionFromContext_NoMiddleware(t *testing.T) {
	req := newReq(http.MethodGet, "/")
	if SessionFromContext(req.Context()) != nil {
		t.Error("SessionFromContext on bare context = non-nil")
	}
}
