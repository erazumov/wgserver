package wg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddPeer_NoPresharedKey(t *testing.T) {
	r := &fakeRunner{}
	if err := AddPeer(r, "wg1", "PUBKEY_X", "10.0.1.2/32", t.TempDir(), ""); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(r.calls))
	}
	want := []string{"set", "wg1", "peer", "PUBKEY_X", "allowed-ips", "10.0.1.2/32"}
	got := r.calls[0].Args
	if r.calls[0].Name != "wg" || !equalStrings(got, want) {
		t.Errorf("call = wg %v, want wg %v", got, want)
	}
}

func TestAddPeer_WithPresharedKey(t *testing.T) {
	r := &fakeRunner{}
	dir := t.TempDir()
	if err := AddPeer(r, "wg1", "PUBKEY_X", "10.0.1.2/32", dir, "PSK_BASE64"); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(r.calls))
	}
	wantPrefix := []string{"set", "wg1", "peer", "PUBKEY_X", "allowed-ips", "10.0.1.2/32", "preshared-key"}
	got := r.calls[0].Args
	if r.calls[0].Name != "wg" || len(got) != len(wantPrefix)+1 {
		t.Fatalf("call = wg %v, want wg <prefix>+1", got)
	}
	for i, s := range wantPrefix {
		if got[i] != s {
			t.Errorf("arg[%d] = %q, want %q", i, got[i], s)
		}
	}
	pskPath := got[len(wantPrefix)]
	// The PSK path must be inside dir and end in a safe-encoded pubkey.
	if !strings.HasPrefix(pskPath, dir) {
		t.Errorf("psk path %q not under dir %q", pskPath, dir)
	}
	wantSuffix := strings.NewReplacer("/", "_", "+", "-").Replace("PUBKEY_X")
	if filepath.Base(pskPath) != wantSuffix {
		t.Errorf("psk file = %q, want %q", filepath.Base(pskPath), wantSuffix)
	}
	// File must contain the PSK followed by a newline (wireguard-tools
	// expects the key on a single line; `wg setconf` tolerates trailing
	// whitespace).
	body, err := os.ReadFile(pskPath)
	if err != nil {
		t.Fatalf("read psk: %v", err)
	}
	if string(body) != "PSK_BASE64\n" {
		t.Errorf("psk body = %q, want %q", body, "PSK_BASE64\n")
	}
	if info, err := os.Stat(pskPath); err != nil || info.Mode().Perm() != 0600 {
		t.Errorf("psk mode = %v, want 0600 (err=%v)", info.Mode().Perm(), err)
	}
}

func TestAddPeer_PresharedKeyCreatesDir(t *testing.T) {
	r := &fakeRunner{}
	dir := filepath.Join(t.TempDir(), "nested", "psk")
	if err := AddPeer(r, "wg1", "PUBKEY_X", "10.0.1.2/32", dir, "PSK"); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("psk dir not created: %v", err)
	}
}

func TestAddPeer_RunnerError(t *testing.T) {
	r := &fakeRunner{runErr: errors.New("set failed")}
	err := AddPeer(r, "wg1", "PUBKEY_X", "10.0.1.2/32", t.TempDir(), "")
	if err == nil {
		t.Fatal("AddPeer: want error, got nil")
	}
}

func TestAddPeer_PSKPathHandlesBase64Chars(t *testing.T) {
	// Base64 includes '+' and '/'. Both must be safe-encoded so the
	// file path is a single component (no subdirs).
	r := &fakeRunner{}
	dir := t.TempDir()
	// Pubkey contains both '+' and '/'.
	pub := "abc+def/ghi="
	if err := AddPeer(r, "wg1", pub, "10.0.1.2/32", dir, "PSK"); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	pskPath := r.calls[0].Args[len(r.calls[0].Args)-1]
	if filepath.Dir(pskPath) != dir {
		t.Errorf("psk path created subdir: dir=%q, full=%q", dir, pskPath)
	}
}

func TestRemovePeer(t *testing.T) {
	r := &fakeRunner{}
	dir := t.TempDir()
	// Pre-create the PSK file the way AddPeer would have.
	pub := "PUBKEY_X"
	if _, err := writePSKFile(dir, pub, "PSK_BASE64"); err != nil {
		t.Fatalf("writePSKFile: %v", err)
	}
	if err := RemovePeer(r, "wg1", dir, pub); err != nil {
		t.Fatalf("RemovePeer: %v", err)
	}
	want := []string{"set", "wg1", "peer", pub, "remove"}
	got := r.calls[0].Args
	if r.calls[0].Name != "wg" || !equalStrings(got, want) {
		t.Errorf("call = wg %v, want wg %v", got, want)
	}
	if _, err := os.Stat(pskFilePath(dir, pub)); !os.IsNotExist(err) {
		t.Errorf("psk file should be removed, stat err = %v", err)
	}
}

func TestRemovePeer_RunnerError(t *testing.T) {
	r := &fakeRunner{runErr: errors.New("remove failed")}
	err := RemovePeer(r, "wg1", t.TempDir(), "PUBKEY_X")
	if err == nil {
		t.Fatal("RemovePeer: want error, got nil")
	}
}

func TestRemovePeer_NoPSKFileIsNotFatal(t *testing.T) {
	// RemovePeer should succeed even if the PSK file is missing —
	// e.g. a peer added without a PSK never had a file written.
	r := &fakeRunner{}
	if err := RemovePeer(r, "wg1", t.TempDir(), "PUBKEY_X"); err != nil {
		t.Fatalf("RemovePeer: %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
