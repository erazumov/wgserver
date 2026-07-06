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
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.github.com"

// ErrNoRelease is returned by LatestRelease when GitHub responds 404
// (no published releases yet) or when the assets array is empty. The
// updater treats this as a no-op, not a failure: it's a normal state
// for a brand-new repo.
var ErrNoRelease = errors.New("no release found")

type Release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

type Asset struct {
	Name string
	URL  string
}

// FindAsset returns the asset whose name is "wgserver-<goos>-<goarch>".
// The matching .sha256 sidecar is NOT included — only the binary.
func (r Release) FindAsset(goos, goarch string) (Asset, bool) {
	want := "wgserver-" + goos + "-" + goarch
	for _, a := range r.Assets {
		if a.Name == want {
			return Asset{Name: a.Name, URL: a.BrowserDownloadURL}, true
		}
	}
	return Asset{}, false
}

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func NewClient() *Client {
	return &Client{
		BaseURL: defaultBaseURL,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) LatestRelease(ctx context.Context, repo string) (Release, error) {
	url := c.BaseURL + "/repos/" + repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, fmt.Errorf("latest release: new request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("latest release: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return Release{}, ErrNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("latest release: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var r Release
	if err := json.Unmarshal(body, &r); err != nil {
		return Release{}, fmt.Errorf("latest release: decode: %w", err)
	}
	if r.TagName == "" {
		return Release{}, ErrNoRelease
	}
	return r, nil
}

// DownloadAndVerify downloads the asset to dest, downloads the sha256
// sidecar from sha256URL, and verifies the digest. On any failure the
// partial file at dest is removed. sha256URL may be empty to skip
// verification (used in tests).
func (c *Client) DownloadAndVerify(ctx context.Context, assetURL, sha256URL, dest string) error {
	if err := download(ctx, c.HTTP, assetURL, dest); err != nil {
		return fmt.Errorf("download asset: %w", err)
	}
	if sha256URL == "" {
		return nil
	}
	actual, err := fileSHA256(dest)
	if err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("hash downloaded: %w", err)
	}
	expect, err := c.fetchSHA256(ctx, sha256URL)
	if err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("download sha256: %w", err)
	}
	if !strings.EqualFold(actual, expect) {
		_ = os.Remove(dest)
		return fmt.Errorf("download verify: sha256 mismatch: got %s, want %s", actual, expect)
	}
	return nil
}

func (c *Client) fetchSHA256(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	// sha256sum format: "<hex>  <filename>\n" (two spaces). Take the
	// first whitespace-delimited token and validate length.
	tok := strings.TrimSpace(strings.SplitN(string(b), " ", 2)[0])
	if len(tok) != 64 {
		return "", fmt.Errorf("malformed sha256 file (got %q)", strings.TrimSpace(string(b)))
	}
	return tok, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func download(ctx context.Context, client *http.Client, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d for %s", resp.StatusCode, url)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = f.Close()
		_ = os.Remove(dest)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return nil
}

// semverCompare returns -1/0/1. "dev" compares equal to anything (caller
// checks separately and skips update for dev). Both inputs must start
// with "v" or the result is meaningless — callers only pass tag_name
// (always "v…") or the literal "dev".
func semverCompare(a, b string) int {
	if a == b {
		return 0
	}
	if a == "dev" || b == "dev" {
		return 0
	}
	ap := strings.TrimPrefix(a, "v")
	bp := strings.TrimPrefix(b, "v")
	as := strings.Split(ap, ".")
	bs := strings.Split(bp, ".")
	for i := 0; i < 3 && i < len(as) && i < len(bs); i++ {
		if as[i] == bs[i] {
			continue
		}
		// numeric compare; no leading zeros in well-formed semver.
		if lessNumeric(as[i], bs[i]) {
			return -1
		}
		return 1
	}
	return 0
}

func lessNumeric(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
