// Command wgserver-updater is the auto-updater. systemd runs it
// hourly via wgserver-updater.timer. It compares the running wgserver
// version (from `wgserver version`, populated at build time via
// -ldflags) to the latest GitHub release, downloads and sha256-
// verifies the new binary, atomically swaps it in, restarts the
// wgserver service, and rolls back if /healthz fails. See
// AGENTS.md invariant: "The updater restarts the service, it does
// not run the app."
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/erazumov/wgserver/internal/config"
	"github.com/erazumov/wgserver/internal/updater"
)

func main() {
	fs := flag.NewFlagSet("wgserver-updater", flag.ExitOnError)
	configPath := fs.String("config", "/etc/wgserver/wgserver.yaml", "path to wgserver YAML config")
	binPath := fs.String("bin", "/usr/local/bin/wgserver", "path to the wgserver binary")
	backupDir := fs.String("backup-dir", "/var/lib/wgserver/.update", "directory for staged downloads and backups")
	fs.Parse(os.Args[1:])

	logger := log.New(os.Stdout, "wgserver-updater: ", log.LstdFlags|log.Lmsgprefix)

	if err := run(logger, *configPath, *binPath, *backupDir); err != nil {
		logger.Printf("error: %v", err)
		os.Exit(1)
	}
}

func run(logger *log.Logger, configPath, binPath, backupDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !cfg.Update.Enabled {
		logger.Printf("update.enabled=false in config; exiting")
		return nil
	}

	currentVer, err := detectCurrentVersion(ctx, binPath)
	if err != nil {
		return fmt.Errorf("detect current version: %w", err)
	}
	logger.Printf("running wgserver %s, latest GitHub release pending", currentVer)

	healthURL := "http://" + cfg.HTTP.Addr + "/healthz"
	if cfg.HTTP.HealthAddr != "" {
		healthURL = "http://" + cfg.HTTP.HealthAddr + "/healthz"
	}

	u := &updater.Updater{
		Repo:        cfg.Update.GitHubRepo,
		CurrentBin:  binPath,
		BackupDir:   backupDir,
		ServiceName: "wgserver",
		HealthURL:   healthURL,
		PinnedVer:   os.Getenv("WGSERVER_VERSION"),
		CurrentVer:  currentVer,
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		Logger:      logger,
	}

	res, err := u.Check(ctx)
	if err != nil {
		// ErrNoRelease is a normal "no published releases" case; log
		// it and exit 0 so systemd doesn't mark the timer as failed.
		if err == updater.ErrNoRelease {
			logger.Printf("no release on GitHub; nothing to do")
			return nil
		}
		return err
	}
	logger.Printf("action=%s from=%s to=%s reason=%q", res.Action, res.FromVersion, res.ToVersion, res.Reason)
	return nil
}

// detectCurrentVersion exec's the installed wgserver binary's `version`
// subcommand and returns its stdout, trimmed. If the binary does not
// exist or fails, returns "dev" — the updater then refuses to act.
func detectCurrentVersion(ctx context.Context, binPath string) (string, error) {
	if _, err := os.Stat(binPath); err != nil {
		return "dev", nil
	}
	out, err := exec.CommandContext(ctx, binPath, "version").Output()
	if err != nil {
		return "dev", nil
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "dev", nil
	}
	return v, nil
}
