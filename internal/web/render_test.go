package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLoadTemplates_AllPages(t *testing.T) {
	tmpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}
	for _, name := range []string{"login.html", "dashboard.html", "peer_new.html"} {
		if _, ok := tmpl.pages[name]; !ok {
			t.Errorf("template %q not loaded", name)
		}
	}
}

func TestRender_LoginContainsForm(t *testing.T) {
	tmpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}
	app := &App{
		Sessions:   NewSessionStore(0),
		CSRFKey:    []byte("k"),
		Templates:  tmpl,
		CookieName: "x",
	}
	req := newReq(http.MethodGet, "/admin/login")
	rec := httptest.NewRecorder()
	app.render(rec, req, "login.html", nil)
	body := rec.Body.String()

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	for _, want := range []string{
		`<form method="post" action="/admin/login"`,
		`name="username"`,
		`name="password"`,
		`name="csrf_token"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("login body missing %q\n---\n%s\n---", want, body)
		}
	}
}

func TestRender_DashboardShowsNoPeers(t *testing.T) {
	tmpl, _ := LoadTemplates()
	app := &App{
		Sessions:   NewSessionStore(0),
		CSRFKey:    []byte("k"),
		Templates:  tmpl,
		CookieName: "x",
	}
	rec := httptest.NewRecorder()
	app.render(rec, newReq(http.MethodGet, "/admin/"), "dashboard.html", nil)
	body := rec.Body.String()
	if !strings.Contains(body, "no peers yet") {
		t.Errorf("dashboard empty: missing 'no peers yet' hint\n%s", body)
	}
}

func TestRender_PeerNewFormHasNameField(t *testing.T) {
	tmpl, _ := LoadTemplates()
	app := &App{
		Sessions:   NewSessionStore(0),
		CSRFKey:    []byte("k"),
		Templates:  tmpl,
		CookieName: "x",
	}
	rec := httptest.NewRecorder()
	app.render(rec, newReq(http.MethodGet, "/admin/peers/new"), "peer_new.html", nil)
	body := rec.Body.String()
	if !strings.Contains(body, `name="name"`) {
		t.Errorf("peer_new: missing name field\n%s", body)
	}
	if !strings.Contains(body, `action="/admin/peers"`) {
		t.Errorf("peer_new: wrong form action\n%s", body)
	}
}

func TestRender_TemplatesReuseLayout(t *testing.T) {
	tmpl, _ := LoadTemplates()
	app := &App{
		Sessions:   NewSessionStore(0),
		CSRFKey:    []byte("k"),
		Templates:  tmpl,
		CookieName: "x",
	}
	for _, name := range []string{"login.html", "dashboard.html", "peer_new.html"} {
		rec := httptest.NewRecorder()
		app.render(rec, newReq(http.MethodGet, "/"), name, nil)
		body := rec.Body.String()
		if !strings.Contains(body, "<!doctype html>") {
			t.Errorf("%s: missing <!doctype html>\n%s", name, body)
		}
		if !strings.Contains(body, `name="csrf-token"`) {
			t.Errorf("%s: missing csrf meta tag (layout not used?)\n%s", name, body)
		}
	}
}

// silence unused import guard.
var _ = bytes.NewBuffer
