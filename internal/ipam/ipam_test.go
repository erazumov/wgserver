package ipam

import (
	"context"
	"database/sql"
	"net"
	"testing"

	"github.com/erazumov/wgserver/internal/store"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/ipam.sqlite")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestAllocate_ReturnsFirstFreeIP(t *testing.T) {
	db := openTestDB(t)
	got, err := Allocate(context.Background(), db, "10.0.1.0/24", "10.0.1.1/24")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got != "10.0.1.2/32" {
		t.Errorf("got %q, want 10.0.1.2/32 (skip network + server)", got)
	}
}

func TestAllocate_SkipsAssignedIPs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if _, err := store.CreatePeer(ctx, db, store.Peer{
		Name: "alice", PublicKey: "k1", PrivateKey: "p1",
		AssignedIP: "10.0.1.2/32", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	if _, err := store.CreatePeer(ctx, db, store.Peer{
		Name: "bob", PublicKey: "k2", PrivateKey: "p2",
		AssignedIP: "10.0.1.3/32", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	got, err := Allocate(ctx, db, "10.0.1.0/24", "10.0.1.1/24")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got != "10.0.1.4/32" {
		t.Errorf("got %q, want 10.0.1.4/32", got)
	}
}

func TestAllocate_SkipsBroadcast(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// /29 has 8 addrs (.0-.7). Fill all 6 usable (.1 server, .2-.6 peers).
	// Only .0 (net) and .7 (broadcast) should remain. Allocate must NOT
	// hand out the broadcast; it should report exhaustion.
	ip := netIP("10.0.1.2")
	for i := 2; i <= 6; i++ {
		if _, err := store.CreatePeer(ctx, db, store.Peer{
			Name: "p", PublicKey: "k" + string(rune('0'+i)),
			PrivateKey: "p", AssignedIP: "10.0.1." + string(rune('0'+i)) + "/32",
			CreatedAt: 1,
		}); err != nil {
			t.Fatalf("CreatePeer: %v", err)
		}
		_ = ip
	}
	_, err := Allocate(ctx, db, "10.0.1.0/29", "10.0.1.1/24")
	if err == nil {
		t.Fatal("want error (no free IPs), got nil — broadcast must not be handed out")
	}
}

func TestIsNetworkOrBroadcast(t *testing.T) {
	_, n, _ := net.ParseCIDR("10.0.1.0/24")
	if !isNetworkOrBroadcast(netIP("10.0.1.0"), n) {
		t.Error("10.0.1.0 should be network")
	}
	if !isNetworkOrBroadcast(netIP("10.0.1.255"), n) {
		t.Error("10.0.1.255 should be broadcast")
	}
	if isNetworkOrBroadcast(netIP("10.0.1.1"), n) {
		t.Error("10.0.1.1 is a host, must not be flagged")
	}
}

func netIP(s string) net.IP {
	return net.ParseIP(s)
}

func TestAllocate_ExhaustedReturnsError(t *testing.T) {
	db := openTestDB(t)
	if _, err := Allocate(context.Background(), db, "10.0.1.0/30", "10.0.1.1/24"); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestAllocate_BadCIDRReturnsError(t *testing.T) {
	db := openTestDB(t)
	if _, err := Allocate(context.Background(), db, "not-a-cidr", ""); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestAllocate_ServerAddrWithoutPrefix(t *testing.T) {
	db := openTestDB(t)
	// Server address without trailing /24 should still be excluded.
	got, err := Allocate(context.Background(), db, "10.0.1.0/24", "10.0.1.1")
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got != "10.0.1.2/32" {
		t.Errorf("got %q, want 10.0.1.2/32", got)
	}
}
