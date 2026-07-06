package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestTelegram_UpsertInsertsAndUpdates(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()

	u := TelegramUser{
		ID:         111,
		Username:   "alice",
		FirstName:  "Alice",
		LastSeenAt: now,
		IsMember:   true,
	}
	if err := UpsertTelegramUser(ctx, db, u); err != nil {
		t.Fatalf("UpsertTelegramUser insert: %v", err)
	}

	got, err := GetTelegramUser(ctx, db, 111)
	if err != nil {
		t.Fatalf("GetTelegramUser: %v", err)
	}
	if got.Username != "alice" || got.FirstName != "Alice" {
		t.Errorf("got %+v, want username=alice first_name=Alice", got)
	}
	if got.QuotaUsed != 0 {
		t.Errorf("QuotaUsed = %d, want 0", got.QuotaUsed)
	}

	u2 := u
	u2.Username = "alice2"
	u2.FirstName = "Alicia"
	u2.LastSeenAt = now + 5
	if err := UpsertTelegramUser(ctx, db, u2); err != nil {
		t.Fatalf("UpsertTelegramUser update: %v", err)
	}

	got2, err := GetTelegramUser(ctx, db, 111)
	if err != nil {
		t.Fatalf("GetTelegramUser 2: %v", err)
	}
	if got2.Username != "alice2" || got2.FirstName != "Alicia" {
		t.Errorf("after update got %+v", got2)
	}
	if got2.QuotaUsed != 0 {
		t.Errorf("QuotaUsed changed on upsert: %d", got2.QuotaUsed)
	}
	if got2.LastSeenAt != now+5 {
		t.Errorf("LastSeenAt = %d, want %d", got2.LastSeenAt, now+5)
	}
}

func TestTelegram_ClaimPeerForTelegramUser_AllowsUpToLimit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := UpsertTelegramUser(ctx, db, TelegramUser{ID: 1, LastSeenAt: time.Now().Unix(), IsMember: true}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	limit := 2
	for i := 1; i <= limit; i++ {
		peer := Peer{
			Name:       "tg-peer",
			PublicKey:  "K",
			PrivateKey: "PV",
			AssignedIP: "10.0.1.2/32",
			CreatedAt:  time.Now().Unix(),
		}
		peer.PublicKey = "K" + string(rune('0'+i))
		peer.PrivateKey = "PV" + string(rune('0'+i))
		peer.AssignedIP = "10.0.1." + string(rune('1'+i)) + "/32"
		_, err := ClaimPeerForTelegramUser(ctx, db, 1, peer, limit)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
	}
	got, err := GetTelegramUser(ctx, db, 1)
	if err != nil {
		t.Fatalf("GetTelegramUser: %v", err)
	}
	if got.QuotaUsed != limit {
		t.Errorf("QuotaUsed = %d, want %d", got.QuotaUsed, limit)
	}
}

func TestTelegram_ClaimPeerForTelegramUser_RejectsOverLimit(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := UpsertTelegramUser(ctx, db, TelegramUser{ID: 2, LastSeenAt: time.Now().Unix(), IsMember: true}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	limit := 1
	peer := Peer{
		Name: "tg-peer", PublicKey: "K1", PrivateKey: "PV1",
		AssignedIP: "10.0.1.2/32", CreatedAt: time.Now().Unix(),
	}
	if _, err := ClaimPeerForTelegramUser(ctx, db, 2, peer, limit); err != nil {
		t.Fatalf("claim 1: %v", err)
	}

	peer2 := Peer{
		Name: "tg-peer", PublicKey: "K2", PrivateKey: "PV2",
		AssignedIP: "10.0.1.3/32", CreatedAt: time.Now().Unix(),
	}
	_, err := ClaimPeerForTelegramUser(ctx, db, 2, peer2, limit)
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Errorf("claim over limit: err = %v, want ErrQuotaExceeded", err)
	}

	got, err := GetTelegramUser(ctx, db, 2)
	if err != nil {
		t.Fatalf("GetTelegramUser: %v", err)
	}
	if got.QuotaUsed != 1 {
		t.Errorf("QuotaUsed = %d, want 1 (failed claim must not increment)", got.QuotaUsed)
	}

	pending, err := ListPeersPendingSync(ctx, db)
	if err != nil {
		t.Fatalf("ListPeersPendingSync: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("pending peers = %d, want 1 (rejected claim must not insert peer)", len(pending))
	}
}

func TestTelegram_ClaimPeerForTelegramUser_UnknownUser(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	peer := Peer{
		Name: "tg-peer", PublicKey: "K1", PrivateKey: "PV1",
		AssignedIP: "10.0.1.2/32", CreatedAt: time.Now().Unix(),
	}
	_, err := ClaimPeerForTelegramUser(ctx, db, 999, peer, 5)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestTelegram_ClaimPeerForTelegramUser_RecordsClaimAndPendingSync(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := UpsertTelegramUser(ctx, db, TelegramUser{ID: 7, LastSeenAt: time.Now().Unix(), IsMember: true}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	peer := Peer{
		Name: "p", PublicKey: "KP", PrivateKey: "PVP",
		AssignedIP: "10.0.1.2/32", CreatedAt: time.Now().Unix(),
	}
	id, err := ClaimPeerForTelegramUser(ctx, db, 7, peer, 3)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	got, err := GetPeerByID(ctx, db, id)
	if err != nil {
		t.Fatalf("GetPeerByID: %v", err)
	}
	if got.CreatedByTelegramUserID == nil || *got.CreatedByTelegramUserID != 7 {
		t.Errorf("CreatedByTelegramUserID = %v, want 7", got.CreatedByTelegramUserID)
	}
	if !got.PendingSync {
		t.Errorf("PendingSync = false, want true (claim returns before WG sync)")
	}
}
