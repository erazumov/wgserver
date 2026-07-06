package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/erazumov/wgserver/internal/config"
	"github.com/erazumov/wgserver/internal/store"
	"github.com/erazumov/wgserver/internal/syncer"
	"github.com/erazumov/wgserver/internal/telegram"
	"github.com/erazumov/wgserver/internal/web"
	"github.com/erazumov/wgserver/internal/wg"
)

// version is set at build time via -ldflags '-X main.version=vX.Y.Z'.
// The auto-updater reads this with `wgserver version` to know whether
// the running binary is older than the latest GitHub release.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		runServe(os.Args[1:])
		return
	}
	switch os.Args[1] {
	case "serve":
		runServe(os.Args[2:])
	case "create-admin":
		runCreateAdmin(os.Args[2:])
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		printUsage()
	default:
		// If the first arg looks like a flag, treat the invocation as
		// `wgserver serve <flags...>` for backward compatibility.
		if len(os.Args[1]) > 0 && os.Args[1][0] == '-' {
			runServe(os.Args[1:])
			return
		}
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: wgserver <command> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  serve            run the web UI (default if no command given)")
	fmt.Fprintln(os.Stderr, "  create-admin     create a new admin user")
	fmt.Fprintln(os.Stderr, "  version          print the binary's version string")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  serve            run the web UI (default if no command given)")
	fmt.Fprintln(os.Stderr, "  create-admin     create a new admin user")
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "/etc/wgserver/wgserver.yaml", "path to YAML config")
	fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if err := serve(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func serve(cfg *config.Config) error {
	db, err := store.Open(cfg.DB.Path)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer db.Close()

	tmpl, err := web.LoadTemplates()
	if err != nil {
		return fmt.Errorf("load templates: %w", err)
	}

	csrfKey, err := readOrCreateKey(filepath.Join(filepath.Dir(cfg.DB.Path), "csrf.key"))
	if err != nil {
		return fmt.Errorf("csrf key: %w", err)
	}

	// Secure (cookie Secure flag) is on only when the admin UI is
	// fronted by TLS. Plain localhost / UNIX-socket deployments set
	// it to false so the cookie doesn't get dropped by the browser.
	secure := cfg.HTTP.UnixSocket == "" &&
		cfg.HTTP.TLSCertFile != "" && cfg.HTTP.TLSKeyFile != ""

	app := &web.App{
		DB:         db,
		Sessions:   web.NewSessionStore(24 * time.Hour),
		CSRFKey:    csrfKey,
		Config:     cfg,
		Templates:  tmpl,
		CookieName: "wgserver_session",
		WGRunner:   wg.ExecRunner{},
		Secure:     secure,
	}

	// The admin listener is selected by config: UNIX socket > TLS-TCP >
	// plain-TCP. /healthz is exposed on a separate plain-TCP listener
	// (HTTPConfig.HealthAddr) so the auto-updater can poll it
	// regardless of admin transport. When admin is plain-TCP and
	// HealthAddr is empty, /healthz rides on the admin listener
	// (legacy single-listener mode).
	adminL, err := web.ListenAdmin(cfg.HTTP)
	if err != nil {
		return fmt.Errorf("admin listener: %w", err)
	}
	adminSrv := &http.Server{
		Handler: app.NewAdminRouter(),
	}

	var healthSrv *http.Server
	var healthL net.Listener
	if cfg.HTTP.HealthAddr != "" {
		healthL, err = web.ListenHealth(cfg.HTTP.HealthAddr)
		if err != nil {
			_ = adminL.Listener.Close()
			return fmt.Errorf("health listener: %w", err)
		}
		healthSrv = &http.Server{Handler: app.NewPublicRouter()}
	}

	// Sync loop reconciles peers.pending_sync=1 against the wg1
	// interface. One immediate RunOnce picks up anything that
	// accumulated while the server was down; subsequent ticks handle
	// new admin/TG actions. The loop shares the same context as the
	// HTTP server so a SIGINT stops both.
	syncCtx, syncCancel := context.WithCancel(context.Background())
	defer syncCancel()
	loop := &syncer.Loop{
		DB:        db,
		Runner:    wg.ExecRunner{},
		Interface: cfg.Clients.Interface,
		Logger:    log.Default(),
		Interval:  5 * time.Second,
	}
	go loop.Run(syncCtx)

	// Telegram bot: long-polls the Bot API, hands .conf files to
	// members of cfg.Telegram.GroupChatID who DM /start. Optional —
	// only started if a bot token is configured. Shares syncCtx so
	// SIGINT stops it too. See AGENTS.md invariant: Telegram users
	// are NOT admins and never touch /admin/*.
	if cfg.Telegram.BotToken != "" {
		bot := &telegram.Bot{
			DB:     db,
			Sender: telegram.NewHTTPSender(cfg.Telegram.BotToken, ""),
			GenKeyPair: func() (string, string, error) {
				kp, err := wg.GenerateKeyPair(wg.ExecRunner{})
				return kp.PrivateKey, kp.PublicKey, err
			},
			Logger:          log.Default(),
			GroupChatID:     cfg.Telegram.GroupChatID,
			PerUserQuota:    cfg.Telegram.PerUserQuota,
			ServerPublicKey: cfg.Clients.PublicKey,
			ServerEndpoint:  cfg.Clients.Endpoint,
			DNSServers:      cfg.Clients.DNSServers,
			CIDR:            cfg.Clients.CIDR,
			ServerAddr:      cfg.Clients.Address,
		}
		go bot.Run(syncCtx)
		log.Printf("wgserver: telegram bot started (group=%d, quota=%d)", cfg.Telegram.GroupChatID, cfg.Telegram.PerUserQuota)
	}

	idleDone := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Print("wgserver: shutting down")
		syncCancel()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = adminSrv.Shutdown(ctx)
		if healthSrv != nil {
			_ = healthSrv.Shutdown(ctx)
		}
		close(idleDone)
	}()

	// If /healthz has no separate listener, mount it on the admin
	// handler so legacy single-listener deployments still expose it.
	if healthSrv == nil {
		adminSrv.Handler = app.NewRouter()
	}

	addr := adminL.Listener.Addr().String()
	log.Printf("wgserver: admin listening on %s (db=%s, sync-iface=%s)", addr, cfg.DB.Path, cfg.Clients.Interface)
	if healthSrv != nil {
		log.Printf("wgserver: healthz on %s", healthL.Addr().String())
	}

	serveErr := make(chan error, 2)
	if healthSrv != nil {
		go func() { serveErr <- healthSrv.Serve(healthL) }()
	}
	go func() {
		if adminL.TLSServer != nil {
			adminSrv.TLSConfig = adminL.TLSServer
			serveErr <- adminSrv.ServeTLS(adminL.Listener, "", "")
		} else {
			serveErr <- adminSrv.Serve(adminL.Listener)
		}
	}()
	for i := 0; i < cap(serveErr); i++ {
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	<-idleDone
	return nil
}

func runCreateAdmin(args []string) {
	fs := flag.NewFlagSet("create-admin", flag.ExitOnError)
	configPath := fs.String("config", "/etc/wgserver/wgserver.yaml", "path to YAML config")
	username := fs.String("username", "", "admin username (required)")
	password := fs.String("password", "", "admin password (prompt if empty)")
	fs.Parse(args)

	if *username == "" {
		fmt.Fprintln(os.Stderr, "-username is required")
		os.Exit(2)
	}
	if *password == "" {
		pw, err := promptPassword("password: ")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		*password = pw
	}
	if *password == "" {
		fmt.Fprintln(os.Stderr, "password cannot be empty")
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	db, err := store.Open(cfg.DB.Path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()

	hash, err := bcrypt.GenerateFromPassword([]byte(*password), bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	id, err := store.CreateAdmin(context.Background(), db, store.Admin{
		Username:     *username,
		PasswordHash: string(hash),
		CreatedAt:    time.Now().Unix(),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("admin %q created (id=%d)\n", *username, id)
}

// readOrCreateKey returns the contents of a key file (32 random bytes),
// creating it with mode 0600 if it does not exist. Used for the CSRF
// HMAC secret so it survives restarts.
func readOrCreateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) < 32 {
			return nil, fmt.Errorf("key file %s too short (%d bytes)", path, len(data))
		}
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		return nil, err
	}
	return buf, nil
}

// promptPassword reads a password from stdin (no echo, requires a TTY).
// In non-interactive contexts (CI, tests), the caller should pass
// -password explicitly. This fallback is plain ReadLine for portability.
func promptPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	rd := bufio.NewReader(os.Stdin)
	line, err := rd.ReadString('\n')
	if err != nil {
		return "", err
	}
	return line[:len(line)-1], nil
}
