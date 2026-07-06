package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"database/sql"

	"golang.org/x/crypto/bcrypt"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "wgserver.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func bcryptHash(t *testing.T, s string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(s), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return string(h)
}

func TestAdmins_CreateAndGetByUsername(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	hash := bcryptHash(t, "s3cret-pa55")
	id, err := CreateAdmin(ctx, db, Admin{
		Username:     "root",
		PasswordHash: hash,
		CreatedAt:    time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	if id == 0 {
		t.Fatal("CreateAdmin returned id=0")
	}

	got, err := GetAdminByUsername(ctx, db, "root")
	if err != nil {
		t.Fatalf("GetAdminByUsername: %v", err)
	}
	if got.ID != id {
		t.Errorf("ID = %d, want %d", got.ID, id)
	}
	if got.Username != "root" {
		t.Errorf("Username = %q, want %q", got.Username, "root")
	}
	if got.PasswordHash != hash {
		t.Errorf("PasswordHash mismatch")
	}
	if got.Disabled {
		t.Errorf("Disabled = true, want false")
	}
}

func TestAdmins_DuplicateUsernameFails(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()
	if _, err := CreateAdmin(ctx, db, Admin{Username: "root", PasswordHash: "h1", CreatedAt: now}); err != nil {
		t.Fatalf("CreateAdmin first: %v", err)
	}
	_, err := CreateAdmin(ctx, db, Admin{Username: "root", PasswordHash: "h2", CreatedAt: now})
	if err == nil {
		t.Fatal("CreateAdmin duplicate: want error, got nil")
	}
}

func TestAdmins_GetByUsername_NotFound(t *testing.T) {
	db := newTestDB(t)
	_, err := GetAdminByUsername(context.Background(), db, "ghost")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetAdminByUsername ghost: err=%v, want sql.ErrNoRows", err)
	}
}

func TestAdmins_List(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	now := time.Now().Unix()
	for _, u := range []string{"alice", "bob", "carol"} {
		if _, err := CreateAdmin(ctx, db, Admin{Username: u, PasswordHash: "h", CreatedAt: now}); err != nil {
			t.Fatalf("CreateAdmin %s: %v", u, err)
		}
	}
	got, err := ListAdmins(ctx, db)
	if err != nil {
		t.Fatalf("ListAdmins: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("len = %d, want 3", len(got))
	}
}

func TestAdmins_Disable(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	id, err := CreateAdmin(ctx, db, Admin{
		Username: "root", PasswordHash: "h", CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("CreateAdmin: %v", err)
	}
	if err := DisableAdmin(ctx, db, id); err != nil {
		t.Fatalf("DisableAdmin: %v", err)
	}
	got, err := GetAdminByUsername(ctx, db, "root")
	if err != nil {
		t.Fatalf("GetAdminByUsername: %v", err)
	}
	if !got.Disabled {
		t.Errorf("Disabled = false, want true")
	}
}
