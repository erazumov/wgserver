package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestPeers_CreateAndGetByID(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	p := Peer{
		Name:         "alice-laptop",
		PublicKey:    "PUBKEY_ALICE",
		PrivateKey:   "PRIVKEY_ALICE",
		PresharedKey: ptrString("PSK_ALICE"),
		AssignedIP:   "10.0.1.2/32",
		CreatedAt:    time.Now().Unix(),
	}
	id, err := CreatePeer(ctx, db, p)
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	if id == 0 {
		t.Fatal("CreatePeer returned id=0")
	}

	got, err := GetPeerByID(ctx, db, id)
	if err != nil {
		t.Fatalf("GetPeerByID: %v", err)
	}
	if got.Name != p.Name {
		t.Errorf("Name = %q, want %q", got.Name, p.Name)
	}
	if got.PublicKey != p.PublicKey {
		t.Errorf("PublicKey mismatch")
	}
	if got.PrivateKey != p.PrivateKey {
		t.Errorf("PrivateKey mismatch")
	}
	if got.PresharedKey == nil || *got.PresharedKey != *p.PresharedKey {
		t.Errorf("PresharedKey = %v, want %q", got.PresharedKey, *p.PresharedKey)
	}
	if got.AssignedIP != p.AssignedIP {
		t.Errorf("AssignedIP mismatch")
	}
	if !got.PendingSync {
		t.Errorf("PendingSync = false, want true on creation")
	}
	if got.Disabled {
		t.Errorf("Disabled = true, want false on creation")
	}
}

func TestPeers_DuplicatePublicKeyFails(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	p := Peer{Name: "p1", PublicKey: "K1", PrivateKey: "PV1", AssignedIP: "10.0.1.2/32", CreatedAt: time.Now().Unix()}
	if _, err := CreatePeer(ctx, db, p); err != nil {
		t.Fatalf("CreatePeer first: %v", err)
	}
	_, err := CreatePeer(ctx, db, Peer{Name: "p2", PublicKey: "K1", PrivateKey: "PV2", AssignedIP: "10.0.1.3/32", CreatedAt: time.Now().Unix()})
	if err == nil {
		t.Fatal("CreatePeer duplicate public_key: want error, got nil")
	}
}

func TestPeers_DuplicateAssignedIPFails(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	p1 := Peer{Name: "p1", PublicKey: "K1", PrivateKey: "PV1", AssignedIP: "10.0.1.2/32", CreatedAt: time.Now().Unix()}
	p2 := Peer{Name: "p2", PublicKey: "K2", PrivateKey: "PV2", AssignedIP: "10.0.1.2/32", CreatedAt: time.Now().Unix()}
	if _, err := CreatePeer(ctx, db, p1); err != nil {
		t.Fatalf("CreatePeer p1: %v", err)
	}
	_, err := CreatePeer(ctx, db, p2)
	if err == nil {
		t.Fatal("CreatePeer duplicate assigned_ip: want error, got nil")
	}
}

func TestPeers_ListPeersPendingSync(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()
	a := Peer{Name: "a", PublicKey: "KA", PrivateKey: "PVA", AssignedIP: "10.0.1.2/32", CreatedAt: now}
	b := Peer{Name: "b", PublicKey: "KB", PrivateKey: "PVB", AssignedIP: "10.0.1.3/32", CreatedAt: now}
	c := Peer{Name: "c", PublicKey: "KC", PrivateKey: "PVC", AssignedIP: "10.0.1.4/32", CreatedAt: now}
	for _, p := range []Peer{a, b, c} {
		if _, err := CreatePeer(ctx, db, p); err != nil {
			t.Fatalf("CreatePeer %s: %v", p.Name, err)
		}
	}
	pending, err := ListPeersPendingSync(ctx, db)
	if err != nil {
		t.Fatalf("ListPeersPendingSync: %v", err)
	}
	if len(pending) != 3 {
		t.Errorf("len(pending) = %d, want 3", len(pending))
	}

	// mark middle one synced, expect 2 left
	for _, p := range pending {
		if p.Name == "b" {
			if err := MarkPeerSynced(ctx, db, p.ID); err != nil {
				t.Fatalf("MarkPeerSynced: %v", err)
			}
		}
	}
	pending2, err := ListPeersPendingSync(ctx, db)
	if err != nil {
		t.Fatalf("ListPeersPendingSync 2: %v", err)
	}
	if len(pending2) != 2 {
		t.Errorf("len(pending) after sync = %d, want 2", len(pending2))
	}
}

func TestPeers_Disable(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	id, err := CreatePeer(ctx, db, Peer{
		Name: "p", PublicKey: "K", PrivateKey: "PV", AssignedIP: "10.0.1.2/32", CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	if err := DisablePeer(ctx, db, id); err != nil {
		t.Fatalf("DisablePeer: %v", err)
	}
	got, err := GetPeerByID(ctx, db, id)
	if err != nil {
		t.Fatalf("GetPeerByID: %v", err)
	}
	if !got.Disabled {
		t.Errorf("Disabled = false, want true")
	}
}

func TestPeers_GetByID_NotFound(t *testing.T) {
	db := newTestDB(t)
	_, err := GetPeerByID(context.Background(), db, 9999)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

func ptrString(s string) *string { return &s }
