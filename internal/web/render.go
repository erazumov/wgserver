package web

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/erazumov/wgserver/internal/store"
)

type pageData struct {
	Title string
	Sess  *Session
	CSRF  string

	Error string
	Peers []store.Peer
}

type Templates struct {
	pages     map[string]*template.Template
	fragments map[string]*template.Template
}

func (t *Templates) Render(w io.Writer, name string, data any) error {
	page, ok := t.pages[name]
	if !ok {
		return fmt.Errorf("template %q not found", name)
	}
	return page.ExecuteTemplate(w, "layout", data)
}

// RenderFragment renders a single named template (defined in a
// fragment file) without the layout. Use for HTMX responses that
// should swap in just the form/content, not the full page.
func (t *Templates) RenderFragment(w io.Writer, file, define string, data any) error {
	frag, ok := t.fragments[file]
	if !ok {
		return fmt.Errorf("fragment %q not found", file)
	}
	return frag.ExecuteTemplate(w, define, data)
}

//go:embed templates/*.html
var templatesFS embed.FS

// loadTemplates walks templates/, registers page templates (each
// producing a `content` define wrapped in the layout) and fragment
// templates (any non-page, non-layout .html producing a named define
// used directly by HTMX responses). Pages are .html files that begin
// with {{define "content"}}. Fragments are any other .html file (e.g.
// peer_new_form.html with {{define "form"}}).
func LoadTemplates() (*Templates, error) {
	base, err := template.ParseFS(templatesFS, "templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("parse layout: %w", err)
	}

	entries, err := fs.ReadDir(templatesFS, "templates")
	if err != nil {
		return nil, fmt.Errorf("read templates: %w", err)
	}

	// First pass: load fragments into the base template. Pages can
	// reference fragments via {{template "name" .}}, and we want those
	// references to resolve in every page clone.
	fragments := map[string]*template.Template{}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "layout.html" {
			continue
		}
		body, err := fs.ReadFile(templatesFS, path.Join("templates", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		if isPageTemplate(string(body)) {
			continue
		}
		if _, err := base.Parse(string(body)); err != nil {
			return nil, fmt.Errorf("parse fragment %s into base: %w", e.Name(), err)
		}
		// Standalone copy for renderFragment (no layout, just the
		// defined name).
		frag, err := template.New(e.Name()).Parse(string(body))
		if err != nil {
			return nil, fmt.Errorf("parse standalone fragment %s: %w", e.Name(), err)
		}
		fragments[e.Name()] = frag
	}

	// Second pass: pages. Each is base.Clone() with "content" added.
	pages := map[string]*template.Template{}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "layout.html" {
			continue
		}
		body, err := fs.ReadFile(templatesFS, path.Join("templates", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		if !isPageTemplate(string(body)) {
			continue
		}
		page, err := base.Clone()
		if err != nil {
			return nil, fmt.Errorf("clone for %s: %w", e.Name(), err)
		}
		if _, err := page.New("content").Parse(string(body)); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		pages[e.Name()] = page
	}
	return &Templates{pages: pages, fragments: fragments}, nil
}

func isPageTemplate(body string) bool {
	return strings.Contains(body, `{{define "content"}}`)
}

func (a *App) render(w http.ResponseWriter, r *http.Request, name string, set func(*pageData)) {
	sess := SessionFromContext(r.Context())
	tok := ""
	if sess != nil {
		tok = SignCSRF(a.CSRFKey, sess.ID)
	}
	pd := pageData{
		Title: defaultTitle(name),
		Sess:  sess,
		CSRF:  tok,
	}
	if set != nil {
		set(&pd)
	}
	if err := a.Templates.Render(w, name, pd); err != nil {
		http.Error(w, fmt.Sprintf("render %s: %v", name, err), http.StatusInternalServerError)
	}
}

func defaultTitle(name string) string {
	switch name {
	case "login.html":
		return "wgserver — login"
	case "dashboard.html":
		return "wgserver — peers"
	case "peer_new.html":
		return "wgserver — new peer"
	default:
		return "wgserver"
	}
}
