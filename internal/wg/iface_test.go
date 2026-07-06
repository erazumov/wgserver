package wg

import (
	"errors"
	"testing"
)

func TestAddPeer_NoPresharedKey(t *testing.T) {
	r := &fakeRunner{}
	if err := AddPeer(r, "wg1", "PUBKEY_X", "10.0.1.2/32", ""); err != nil {
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
	if err := AddPeer(r, "wg1", "PUBKEY_X", "10.0.1.2/32", "PSK_BASE64"); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	want := []string{"set", "wg1", "peer", "PUBKEY_X", "allowed-ips", "10.0.1.2/32", "preshared-key", "PSK_BASE64"}
	got := r.calls[0].Args
	if r.calls[0].Name != "wg" || !equalStrings(got, want) {
		t.Errorf("call = wg %v, want wg %v", got, want)
	}
}

func TestAddPeer_RunnerError(t *testing.T) {
	r := &fakeRunner{runErr: errors.New("set failed")}
	err := AddPeer(r, "wg1", "PUBKEY_X", "10.0.1.2/32", "")
	if err == nil {
		t.Fatal("AddPeer: want error, got nil")
	}
}

func TestRemovePeer(t *testing.T) {
	r := &fakeRunner{}
	if err := RemovePeer(r, "wg1", "PUBKEY_X"); err != nil {
		t.Fatalf("RemovePeer: %v", err)
	}
	want := []string{"set", "wg1", "peer", "PUBKEY_X", "remove"}
	got := r.calls[0].Args
	if r.calls[0].Name != "wg" || !equalStrings(got, want) {
		t.Errorf("call = wg %v, want wg %v", got, want)
	}
}

func TestRemovePeer_RunnerError(t *testing.T) {
	r := &fakeRunner{runErr: errors.New("remove failed")}
	err := RemovePeer(r, "wg1", "PUBKEY_X")
	if err == nil {
		t.Fatal("RemovePeer: want error, got nil")
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
