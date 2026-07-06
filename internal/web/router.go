package web

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter returns the combined handler with public and admin routes
// wired up. Kept for tests and for the legacy single-listener mode
// where /healthz and /admin share one port. Production deployments
// that need a separate /healthz listener (UNIX socket, TLS) should
// use NewPublicRouter + NewAdminRouter.
func (a *App) NewRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	a.registerPublicRoutes(r)
	a.registerAdminRoutes(r)
	return r
}

// NewPublicRouter returns just the /healthz handler. Used when the
// admin UI is bound to a UNIX socket (or behind TLS) and /healthz
// must be reachable on a separate, plain-HTTP TCP port for the
// auto-updater to poll.
func (a *App) NewPublicRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	a.registerPublicRoutes(r)
	return r
}

// NewAdminRouter returns just the /admin/* handler. Does not include
// /healthz — use NewPublicRouter for that.
func (a *App) NewAdminRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	a.registerAdminRoutes(r)
	return r
}

func (a *App) registerPublicRoutes(r chi.Router) {
	r.Get("/healthz", a.Healthz)
}

func (a *App) registerAdminRoutes(r chi.Router) {
	r.Route("/admin", func(r chi.Router) {
		// /admin/login is reachable without auth.
		r.Get("/login", a.LoginGet)
		r.Post("/login", a.LoginPost)

		// Everything else requires a session.
		r.Group(func(r chi.Router) {
			r.Use(a.LoadSession)
			r.Use(a.RequireAdmin)

			// State-changing routes also require CSRF.
			r.Group(func(r chi.Router) {
				r.Use(a.RequireCSRF)

				r.Post("/logout", a.LogoutPost)
				r.Post("/peers", a.PeerNewPost)
				r.Post("/peers/{id}/revoke", a.PeerRevokePost)
			})

			r.Get("/", a.Dashboard)
			r.Get("/peers/new", a.PeerNewGet)
			r.Get("/peers/{id}/config", a.PeerConfigGet)
		})
	})
}
