package web

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/erazumov/wgserver/internal/ipam"
	"github.com/erazumov/wgserver/internal/store"
	"github.com/erazumov/wgserver/internal/wg"
)

func (a *App) Healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func (a *App) LoginGet(w http.ResponseWriter, r *http.Request) {
	if SessionFromContext(r.Context()) != nil {
		http.Redirect(w, r, "/admin/", http.StatusFound)
		return
	}
	a.render(w, r, "login.html", nil)
}

func (a *App) LoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	if username == "" || password == "" {
		a.render(w, r, "login.html", func(pd *pageData) { pd.Error = "invalid credentials" })
		return
	}
	admin, err := store.GetAdminByUsername(r.Context(), a.DB, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || admin.Disabled {
			a.render(w, r, "login.html", func(pd *pageData) { pd.Error = "invalid credentials" })
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if admin.Disabled {
		a.render(w, r, "login.html", func(pd *pageData) { pd.Error = "invalid credentials" })
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(admin.PasswordHash), []byte(password)); err != nil {
		a.render(w, r, "login.html", func(pd *pageData) { pd.Error = "invalid credentials" })
		return
	}
	sess, err := a.Sessions.Create(admin.ID, admin.Username)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     a.CookieName,
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.Secure,
		SameSite: http.SameSiteLaxMode,
		Expires:  sess.ExpiresAt,
	})
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

func (a *App) LogoutPost(w http.ResponseWriter, r *http.Request) {
	if sess := SessionFromContext(r.Context()); sess != nil {
		a.Sessions.Destroy(sess.ID)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     a.CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.Secure,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}

func (a *App) Dashboard(w http.ResponseWriter, r *http.Request) {
	peers, err := store.ListPeers(r.Context(), a.DB)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.render(w, r, "dashboard.html", func(pd *pageData) { pd.Peers = peers })
}

func (a *App) PeerNewGet(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "peer_new.html", nil)
}

func (a *App) PeerNewPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		if isHTMX(r) {
			a.renderFormFragment(w, r, func(pd *pageData) { pd.Error = "name required" })
			return
		}
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}

	sess := SessionFromContext(r.Context())
	adminID := sess.AdminID

	ip, err := ipam.Allocate(r.Context(), a.DB, a.Config.Clients.CIDR, a.Config.Clients.Address)
	if err != nil {
		http.Error(w, "ip allocation: "+err.Error(), http.StatusInternalServerError)
		return
	}

	kp, err := wg.GenerateKeyPair(a.WGRunner)
	if err != nil {
		http.Error(w, "keygen: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var psk *string
	if r.FormValue("generate_psk") == "on" || r.FormValue("generate_psk") == "1" {
		pskVal, err := wg.GeneratePresharedKey(a.WGRunner)
		if err != nil {
			http.Error(w, "psk: "+err.Error(), http.StatusInternalServerError)
			return
		}
		psk = &pskVal
	}

	now := time.Now().Unix()
	_, err = store.CreatePeer(r.Context(), a.DB, store.Peer{
		Name:                    name,
		PublicKey:               kp.PublicKey,
		PrivateKey:              kp.PrivateKey,
		PresharedKey:            psk,
		AssignedIP:              ip,
		CreatedAt:               now,
		CreatedByAdminID:        &adminID,
		CreatedByTelegramUserID: nil,
	})
	if err != nil {
		http.Error(w, "create: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Note: pending_sync stays 1. The sync loop (next iteration) will pick
	// it up and call `wg set` against clients.interface. If WG fails, the
	// row stays pending_sync=1 and the loop retries — never silently
	// desync. See AGENTS.md invariant.
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", "/admin/")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

// isHTMX reports whether the request came from HTMX (HX-Request: true).
func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// renderFormFragment renders the peer-creation form fragment with
// CSRF wired up, plus any error text from set. Used for HTMX error
// responses so htmx can swap the form in place.
func (a *App) renderFormFragment(w http.ResponseWriter, r *http.Request, set func(*pageData)) {
	sess := SessionFromContext(r.Context())
	tok := ""
	if sess != nil {
		tok = SignCSRF(a.CSRFKey, sess.ID)
	}
	pd := pageData{
		Title: defaultTitle("peer_new.html"),
		Sess:  sess,
		CSRF:  tok,
	}
	if set != nil {
		set(&pd)
	}
	if err := a.Templates.RenderFragment(w, "peer_new_form.html", "form", pd); err != nil {
		http.Error(w, fmt.Sprintf("render fragment: %v", err), http.StatusInternalServerError)
	}
}

func (a *App) PeerRevokePost(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := store.DisablePeer(r.Context(), a.DB, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// DisablePeer also sets pending_sync=1 so the sync loop calls
	// `wg set wg1 peer <pubkey> remove`. (See AGENTS.md invariant.)
	http.Redirect(w, r, "/admin/", http.StatusFound)
}

func (a *App) PeerConfigGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	p, err := store.GetPeerByID(r.Context(), a.DB, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	conf := wg.GenerateClientConfig(wg.ClientConfig{
		ClientPrivateKey: p.PrivateKey,
		ClientAddress:    p.AssignedIP,
		DNSServers:       a.Config.Clients.DNSServers,
		ServerPublicKey:  a.Config.Clients.PublicKey,
		ServerEndpoint:   a.Config.Clients.Endpoint,
		AllowedIPs:       "0.0.0.0/0, ::/0",
		Keepalive:        25,
	})
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.conf"`, p.Name))
	_, _ = w.Write([]byte(conf))
}
