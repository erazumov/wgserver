package web

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/erazumov/wgserver/internal/config"
	"github.com/erazumov/wgserver/internal/wg"
)

type ctxKey int

const sessionCtxKey ctxKey = 1

type App struct {
	DB        *sql.DB
	Sessions  *SessionStore
	CSRFKey   []byte
	Config    *config.Config
	Templates *Templates
	WGRunner  wg.Runner

	CookieName string
	Secure     bool
}

func SessionFromContext(ctx context.Context) *Session {
	v, _ := ctx.Value(sessionCtxKey).(*Session)
	return v
}

func (a *App) LoadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(a.CookieName)
		if err == nil {
			if sess, ok := a.Sessions.Get(c.Value); ok {
				ctx := context.WithValue(r.Context(), sessionCtxKey, sess)
				r = r.WithContext(ctx)
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if SessionFromContext(r.Context()) == nil {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireCSRF verifies the CSRF token on state-changing methods. The
// token may be supplied either as the `X-CSRF-Token` header (used by
// future HTMX) or as a `csrf_token` form field.
func (a *App) RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		sess := SessionFromContext(r.Context())
		if sess == nil {
			http.Error(w, "no session", http.StatusBadRequest)
			return
		}
		tok := r.Header.Get("X-CSRF-Token")
		if tok == "" {
			tok = r.FormValue("csrf_token")
		}
		if !VerifyCSRF(a.CSRFKey, sess.ID, tok) {
			http.Error(w, "invalid csrf token", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}
