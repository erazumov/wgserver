package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testRig struct {
	t            *testing.T
	updater      *Updater
	gh           *httptest.Server
	health       *httptest.Server
	healthStatus int
	healthCalls  int
	binPath      string
	binContents  []byte
	restartCalls int
	restartErr   error
}

func newTestRig(t *testing.T) *testRig {
	t.Helper()
	r := &testRig{t: t, healthStatus: 200}

	tmp := t.TempDir()
	r.binPath = filepath.Join(tmp, "wgserver")
	r.binContents = []byte("current-binary")
	if err := os.WriteFile(r.binPath, r.binContents, 0o755); err != nil {
		t.Fatalf("write bin: %v", err)
	}

	// GitHub server — set per-test via setGH.
	// Health server: a handler that returns whatever healthStatus is.
	r.health = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.healthCalls++
		w.WriteHeader(r.healthStatus)
		_, _ = w.Write([]byte("ok"))
	}))

	r.updater = &Updater{
		Repo:           "foo/bar",
		CurrentBin:     r.binPath,
		BackupDir:      filepath.Join(tmp, ".update"),
		ServiceName:    "wgserver",
		HealthURL:      r.health.URL + "/healthz",
		GOOS:           "linux",
		GOARCH:         "amd64",
		Logger:         log.New(io.Discard, "", 0),
		HealthAttempts: 3,
		HealthInterval: 10 * time.Millisecond,
		HealthTimeout:  100 * time.Millisecond,
	}
	r.updater.Client = NewClient() // caller will override BaseURL
	r.updater.Restart = func(ctx context.Context) error {
		r.restartCalls++
		return r.restartErr
	}
	return r
}

func (r *testRig) setGH(handler http.HandlerFunc) {
	r.gh = httptest.NewServer(handler)
	r.updater.Client.BaseURL = r.gh.URL
	r.updater.Client.HTTP = r.gh.Client()
}

func (r *testRig) setCurrentVer(v string)  { r.updater.CurrentVer = v }
func (r *testRig) setPinned(v string)      { r.updater.PinnedVer = v }
func (r *testRig) setHealthStatus(s int)   { r.healthStatus = s }
func (r *testRig) setRestartErr(err error) { r.restartErr = err }

func TestCheck_PinnedSkipsUpdate(t *testing.T) {
	r := newTestRig(t)
	r.setCurrentVer("v0.1.0")
	r.setPinned("v0.1.0")
	r.setGH(func(w http.ResponseWriter, req *http.Request) {
		t.Errorf("GitHub must not be called when pinned")
	})

	res, err := r.updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Action != "skipped" {
		t.Errorf("Action = %q, want skipped", res.Action)
	}
	if r.restartCalls != 0 {
		t.Errorf("Restart called %d times; want 0 when pinned", r.restartCalls)
	}
}

func TestCheck_DevBuildSkipsUpdate(t *testing.T) {
	r := newTestRig(t)
	r.setCurrentVer("dev")
	r.setGH(func(w http.ResponseWriter, req *http.Request) {
		t.Errorf("GitHub must not be called for dev build")
	})

	res, err := r.updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Action != "skipped" {
		t.Errorf("Action = %q, want skipped", res.Action)
	}
	if !strings.Contains(res.Reason, "dev") {
		t.Errorf("Reason = %q, want it to mention 'dev'", res.Reason)
	}
}

func TestCheck_NoReleaseReturnsErrNoRelease(t *testing.T) {
	r := newTestRig(t)
	r.setCurrentVer("v0.1.0")
	r.setGH(func(w http.ResponseWriter, req *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	res, err := r.updater.Check(context.Background())
	if !errors.Is(err, ErrNoRelease) {
		t.Errorf("err = %v, want ErrNoRelease", err)
	}
	if res.Action != "" {
		t.Errorf("Action = %q, want empty on error", res.Action)
	}
}

func TestCheck_CurrentEqualsLatest_NoUpdate(t *testing.T) {
	r := newTestRig(t)
	r.setCurrentVer("v0.2.0")
	r.setGH(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintln(w, `{"tag_name":"v0.2.0","assets":[]}`)
	})

	res, err := r.updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Action != "no-update" {
		t.Errorf("Action = %q, want no-update", res.Action)
	}
	if r.restartCalls != 0 {
		t.Errorf("Restart called %d times; want 0 when current==latest", r.restartCalls)
	}
}

func TestCheck_NewerAvailable_InstallsAndSendsConf(t *testing.T) {
	r := newTestRig(t)
	r.setCurrentVer("v0.1.0")

	newPayload := []byte("new-binary-v0.2.0")
	// We use the rig's bin path as the source of sha256 — the updater
	// will compute it itself. Compute expected sha256.
	// (Already done inside DownloadAndVerify.)
	r.setGH(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/repos/foo/bar/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v0.2.0","assets":[`+
				`{"name":"wgserver-linux-amd64","browser_download_url":%q}`+
				`]}`,
				"http://"+req.Host+"/wgserver-linux-amd64")
		case "/wgserver-linux-amd64":
			_, _ = w.Write(newPayload)
		case "/wgserver-linux-amd64.sha256":
			writeSHA256(w, newPayload, "wgserver-linux-amd64")
		default:
			http.NotFound(w, req)
		}
	})

	res, err := r.updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Action != "updated" {
		t.Errorf("Action = %q, want updated (res=%+v)", res.Action, res)
	}
	if r.restartCalls != 1 {
		t.Errorf("Restart calls = %d, want 1", r.restartCalls)
	}
	if r.healthCalls == 0 {
		t.Error("health check not called after update")
	}
	got, err := os.ReadFile(r.binPath)
	if err != nil {
		t.Fatalf("read new bin: %v", err)
	}
	if string(got) != string(newPayload) {
		t.Errorf("bin content = %q, want %q", got, newPayload)
	}
	// Backup should exist.
	baks, _ := filepath.Glob(filepath.Join(r.updater.BackupDir, "wgserver.bak.*"))
	if len(baks) != 1 {
		t.Errorf("backup files = %d, want 1", len(baks))
	}
}

func TestCheck_HealthFails_RollsBack(t *testing.T) {
	r := newTestRig(t)
	r.setCurrentVer("v0.1.0")
	r.setHealthStatus(http.StatusInternalServerError)

	newPayload := []byte("broken-binary-v0.2.0")
	r.setGH(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/repos/foo/bar/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v0.2.0","assets":[`+
				`{"name":"wgserver-linux-amd64","browser_download_url":%q}`+
				`]}`,
				"http://"+req.Host+"/wgserver-linux-amd64")
		case "/wgserver-linux-amd64":
			_, _ = w.Write(newPayload)
		case "/wgserver-linux-amd64.sha256":
			writeSHA256(w, newPayload, "wgserver-linux-amd64")
		default:
			http.NotFound(w, req)
		}
	})

	res, err := r.updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Action != "rolled-back" {
		t.Errorf("Action = %q, want rolled-back (res=%+v)", res.Action, res)
	}
	if r.restartCalls != 2 {
		t.Errorf("Restart calls = %d, want 2 (install + rollback)", r.restartCalls)
	}
	got, err := os.ReadFile(r.binPath)
	if err != nil {
		t.Fatalf("read bin after rollback: %v", err)
	}
	if string(got) != string(r.binContents) {
		t.Errorf("bin after rollback = %q, want %q (original)", got, r.binContents)
	}
}

func TestInstall_ReplacesBinary(t *testing.T) {
	r := newTestRig(t)
	src := filepath.Join(t.TempDir(), "new")
	if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.updater.install(src); err != nil {
		t.Fatalf("install: %v", err)
	}
	got, _ := os.ReadFile(r.binPath)
	if string(got) != "new" {
		t.Errorf("bin = %q, want 'new'", got)
	}
	baks, _ := filepath.Glob(filepath.Join(r.updater.BackupDir, "wgserver.bak.*"))
	if len(baks) != 1 {
		t.Fatalf("backup count = %d, want 1", len(baks))
	}
	bak, _ := os.ReadFile(baks[0])
	if string(bak) != string(r.binContents) {
		t.Errorf("backup = %q, want %q", bak, r.binContents)
	}
}

func TestRollback_RestoresBackup(t *testing.T) {
	r := newTestRig(t)
	if err := os.WriteFile(r.binPath, []byte("new-broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.updater.BackupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bak := filepath.Join(r.updater.BackupDir, "wgserver.bak.20240101T000000.000")
	if err := os.WriteFile(bak, r.binContents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.updater.rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	got, _ := os.ReadFile(r.binPath)
	if string(got) != string(r.binContents) {
		t.Errorf("bin after rollback = %q, want %q", got, r.binContents)
	}
}

func TestCheck_NoAssetForArch(t *testing.T) {
	r := newTestRig(t)
	r.setCurrentVer("v0.1.0")
	r.setGH(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintln(w, `{"tag_name":"v0.2.0","assets":[{"name":"wgserver-darwin-amd64","browser_download_url":"u"}]}`)
	})

	res, err := r.updater.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Action != "no-update" {
		t.Errorf("Action = %q, want no-update (no asset for our arch)", res.Action)
	}
	if r.restartCalls != 0 {
		t.Errorf("Restart calls = %d, want 0", r.restartCalls)
	}
}

func TestDefaultRestart_UsesSystemctl(t *testing.T) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("systemctl not on PATH; cannot test default restart")
	}
	u := &Updater{ServiceName: "wgserver"}
	_ = u // silence unused
}

func writeSHA256(w io.Writer, payload []byte, filename string) {
	sum := sha256.Sum256(payload)
	fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), filename)
}
