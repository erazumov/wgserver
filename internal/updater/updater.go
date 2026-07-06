package updater

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Result describes what Check did. Action is one of:
//
//	"skipped"      — pinned to a version, dev build, or no work to do
//	"no-update"    — checked GitHub, current version is already up to date
//	"updated"      — new binary installed and healthy
//	"rolled-back"  — new binary installed but unhealthy; previous binary restored
//
// "error" is set when the action could not be carried to completion.
type Result struct {
	Action       string
	FromVersion  string
	ToVersion    string
	Reason       string
	HealthTries  int
	RollbackPath string
}

// Updater orchestrates a single update check. It is intended to be run
// by a systemd oneshot service on a timer.
type Updater struct {
	Repo        string
	CurrentBin  string
	BackupDir   string
	ServiceName string
	HealthURL   string
	PinnedVer   string
	CurrentVer  string
	GOOS        string
	GOARCH      string
	Logger      *log.Logger

	HealthAttempts int
	HealthInterval time.Duration
	HealthTimeout  time.Duration

	// Test seams. Defaults are set in Check if nil.
	Client *Client
	// Restart restarts the wgserver service. Default: systemctl.
	Restart func(ctx context.Context) error
}

// Check performs one update cycle. See the Result type for outcomes.
func (u *Updater) Check(ctx context.Context) (Result, error) {
	if u.Logger == nil {
		u.Logger = log.Default()
	}
	if u.Client == nil {
		u.Client = NewClient()
	}
	if u.Restart == nil {
		svc := u.ServiceName
		u.Restart = func(ctx context.Context) error {
			return exec.CommandContext(ctx, "systemctl", "restart", svc).Run()
		}
	}
	if u.HealthAttempts == 0 {
		u.HealthAttempts = 30
	}
	if u.HealthInterval == 0 {
		u.HealthInterval = time.Second
	}
	if u.HealthTimeout == 0 {
		u.HealthTimeout = time.Second
	}

	res := Result{FromVersion: u.CurrentVer}

	if u.PinnedVer != "" {
		res.Action = "skipped"
		res.Reason = "pinned to " + u.PinnedVer
		return res, nil
	}
	if u.CurrentVer == "dev" || u.CurrentVer == "" {
		res.Action = "skipped"
		res.Reason = "dev build; not eligible for auto-update"
		return res, nil
	}

	rel, err := u.Client.LatestRelease(ctx, u.Repo)
	if err != nil {
		return res, err
	}
	res.ToVersion = rel.TagName

	cmp := semverCompare(u.CurrentVer, rel.TagName)
	if cmp >= 0 {
		res.Action = "no-update"
		res.Reason = fmt.Sprintf("current %s >= latest %s", u.CurrentVer, rel.TagName)
		return res, nil
	}

	asset, ok := rel.FindAsset(u.GOOS, u.GOARCH)
	if !ok {
		res.Action = "no-update"
		res.Reason = fmt.Sprintf("no asset for %s/%s in release %s", u.GOOS, u.GOARCH, rel.TagName)
		return res, nil
	}

	if err := os.MkdirAll(u.BackupDir, 0o755); err != nil {
		return res, fmt.Errorf("backup dir: %w", err)
	}
	dlPath := filepath.Join(u.BackupDir, "wgserver.new")
	if err := u.Client.DownloadAndVerify(ctx, asset.URL, asset.URL+".sha256", dlPath); err != nil {
		return res, fmt.Errorf("download: %w", err)
	}
	if err := os.Chmod(dlPath, 0o755); err != nil {
		return res, fmt.Errorf("chmod: %w", err)
	}

	if err := u.install(dlPath); err != nil {
		return res, fmt.Errorf("install: %w", err)
	}

	if err := u.Restart(ctx); err != nil {
		return res, fmt.Errorf("restart: %w", err)
	}

	if err := u.healthCheck(ctx); err != nil {
		u.Logger.Printf("updater: health check failed after update to %s: %v — rolling back", rel.TagName, err)
		if rbErr := u.rollback(); rbErr != nil {
			return res, fmt.Errorf("health failed (%v) and rollback failed: %w", err, rbErr)
		}
		if rbErr := u.Restart(ctx); rbErr != nil {
			return res, fmt.Errorf("health failed and rollback restart failed: %w", rbErr)
		}
		res.Action = "rolled-back"
		res.Reason = err.Error()
		return res, nil
	}

	res.Action = "updated"
	u.Logger.Printf("updater: %s -> %s", u.CurrentVer, rel.TagName)
	return res, nil
}

// install copies src onto CurrentBin after backing up the existing
// binary. The swap is atomic on POSIX: write to a sibling path then
// rename(2) over the destination.
func (u *Updater) install(src string) error {
	if err := os.MkdirAll(u.BackupDir, 0o755); err != nil {
		return err
	}
	bakPath := filepath.Join(u.BackupDir, "wgserver.bak."+time.Now().UTC().Format("20060102T150405.000"))
	// Backup is best-effort: if the current binary doesn't exist, we
	// can't restore later, so install proceeds without a backup and a
	// subsequent rollback will fail loudly. This matches a fresh
	// install case, which shouldn't happen via the updater.
	if _, err := os.Stat(u.CurrentBin); err == nil {
		if err := copyFile(u.CurrentBin, bakPath, 0o755); err != nil {
			return fmt.Errorf("backup %s -> %s: %w", u.CurrentBin, bakPath, err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(u.CurrentBin), ".wgserver-new-*")
	if err != nil {
		return fmt.Errorf("temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := copyFile(src, tmpName, 0o755); err != nil {
		return fmt.Errorf("copy new: %w", err)
	}
	if err := os.Rename(tmpName, u.CurrentBin); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// rollback restores the most recent backup. On failure (e.g. no
// backup exists), the caller surfaces the error to systemd and the
// operator must intervene.
func (u *Updater) rollback() error {
	matches, err := filepath.Glob(filepath.Join(u.BackupDir, "wgserver.bak.*"))
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		return fmt.Errorf("no backup found in %s", u.BackupDir)
	}
	// Pick the most recent (lexicographic, which works for our timestamp
	// format) and copy it back.
	latest := matches[len(matches)-1]
	tmp, err := os.CreateTemp(filepath.Dir(u.CurrentBin), ".wgserver-rb-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	_ = tmp.Close()
	if err := copyFile(latest, tmpName, 0o755); err != nil {
		return fmt.Errorf("copy backup: %w", err)
	}
	if err := os.Rename(tmpName, u.CurrentBin); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

func (u *Updater) healthCheck(ctx context.Context) error {
	client := &http.Client{Timeout: u.HealthTimeout}
	var lastErr error
	for i := 0; i < u.HealthAttempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.HealthURL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(u.HealthInterval):
		}
	}
	return fmt.Errorf("health check failed after %d attempts: %w", u.HealthAttempts, lastErr)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
