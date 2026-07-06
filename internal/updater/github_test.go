// Package updater implements wgserver's auto-update flow: poll
// GitHub Releases for a newer version of the binary, download it,
// verify sha256, atomically swap, restart the service, and roll back
// if /healthz fails. See AGENTS.md invariant: "The updater restarts
// the service, it does not run the app."
package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClient_LatestRelease_ParsesJSON(t *testing.T) {
	release := map[string]any{
		"tag_name": "v0.2.0",
		"assets": []map[string]any{
			{
				"name":                 "wgserver-linux-amd64",
				"browser_download_url": "https://example.invalid/wgserver-linux-amd64",
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/foo/bar/releases/latest" {
			t.Errorf("path = %q, want /repos/foo/bar/releases/latest", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(release)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	got, err := c.LatestRelease(context.Background(), "foo/bar")
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if got.TagName != "v0.2.0" {
		t.Errorf("TagName = %q, want v0.2.0", got.TagName)
	}
	if len(got.Assets) != 1 {
		t.Fatalf("Assets = %d, want 1", len(got.Assets))
	}
}

func TestClient_LatestRelease_404ReturnsErrNoRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Not Found", http.StatusNotFound)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := c.LatestRelease(context.Background(), "x/y")
	if !errors.Is(err, ErrNoRelease) {
		t.Errorf("err = %v, want ErrNoRelease", err)
	}
}

func TestClient_LatestRelease_5xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	_, err := c.LatestRelease(context.Background(), "x/y")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if errors.Is(err, ErrNoRelease) {
		t.Errorf("5xx should NOT be ErrNoRelease, got %v", err)
	}
}

func TestRelease_FindAsset_MatchesGoosGoarch(t *testing.T) {
	r := Release{
		Assets: []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		}{
			{Name: "wgserver-linux-amd64", BrowserDownloadURL: "u1"},
			{Name: "wgserver-linux-arm64", BrowserDownloadURL: "u2"},
			{Name: "wgserver-linux-amd64.sha256", BrowserDownloadURL: "u3"},
		},
	}
	a, ok := r.FindAsset("linux", "amd64")
	if !ok {
		t.Fatal("FindAsset: not found")
	}
	if a.Name != "wgserver-linux-amd64" || a.URL != "u1" {
		t.Errorf("got %+v, want wgserver-linux-amd64/u1", a)
	}
}

func TestRelease_FindAsset_MissingReturnsFalse(t *testing.T) {
	r := Release{}
	if _, ok := r.FindAsset("linux", "amd64"); ok {
		t.Error("FindAsset on empty release: want false")
	}
}

func TestClient_DownloadAndVerify_Success(t *testing.T) {
	payload := []byte("binary-contents-v0.2.0")
	sum := sha256.Sum256(payload)
	hexsum := hex.EncodeToString(sum[:])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/wgserver-linux-amd64":
			_, _ = w.Write(payload)
		case "/wgserver-linux-amd64.sha256":
			// sha256sum format: "<hex>  <filename>\n"
			fmt.Fprintf(w, "%s  wgserver-linux-amd64\n", hexsum)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	dest := filepath.Join(t.TempDir(), "wgserver")
	if err := c.DownloadAndVerify(context.Background(),
		srv.URL+"/wgserver-linux-amd64",
		srv.URL+"/wgserver-linux-amd64.sha256",
		dest); err != nil {
		t.Fatalf("DownloadAndVerify: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("downloaded = %q, want %q", got, payload)
	}
}

func TestClient_DownloadAndVerify_MismatchRemovesDest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/wgserver-linux-amd64":
			_, _ = w.Write([]byte("real contents"))
		case "/wgserver-linux-amd64.sha256":
			fmt.Fprintf(w, "%s  wgserver-linux-amd64\n", strings.Repeat("0", 64))
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	dest := filepath.Join(t.TempDir(), "wgserver")
	err := c.DownloadAndVerify(context.Background(),
		srv.URL+"/wgserver-linux-amd64",
		srv.URL+"/wgserver-linux-amd64.sha256",
		dest)
	if err == nil {
		t.Fatal("want sha256 mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("err = %v, want it to mention 'sha256 mismatch'", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dest still exists after mismatch; want it removed")
	}
}

func TestClient_DownloadAndVerify_MalformedSHAFileErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/wgserver-linux-amd64":
			_, _ = w.Write([]byte("anything"))
		case "/wgserver-linux-amd64.sha256":
			_, _ = w.Write([]byte("not-a-hex-digest\n"))
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	dest := filepath.Join(t.TempDir(), "wgserver")
	err := c.DownloadAndVerify(context.Background(),
		srv.URL+"/wgserver-linux-amd64",
		srv.URL+"/wgserver-linux-amd64.sha256",
		dest)
	if err == nil {
		t.Fatal("want error on malformed sha256, got nil")
	}
}

func TestClient_DownloadAndVerify_AssetNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	dest := filepath.Join(t.TempDir(), "wgserver")
	err := c.DownloadAndVerify(context.Background(),
		srv.URL+"/wgserver-linux-amd64",
		srv.URL+"/wgserver-linux-amd64.sha256",
		dest)
	if err == nil {
		t.Fatal("want error on 404, got nil")
	}
}

func TestSemverCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.1.0", "v0.1.0", 0},
		{"v0.1.0", "v0.2.0", -1},
		{"v0.2.0", "v0.1.0", 1},
		{"v0.10.0", "v0.2.0", 1},
		{"v1.0.0", "v0.99.0", 1},
		{"dev", "v0.1.0", 0}, // dev is never "newer" — handled by caller, this is a degenerate input
	}
	for _, c := range cases {
		if got := semverCompare(c.a, c.b); got != c.want {
			t.Errorf("semverCompare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

var _ = time.Second
var _ = io.Discard
