package web

import (
	"testing"
	"time"
)

func TestSessionStore_CreateAndGet(t *testing.T) {
	store := NewSessionStore(time.Hour)
	s, err := store.Create(42, "root")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.ID == "" {
		t.Fatal("Session.ID is empty")
	}
	if s.AdminID != 42 {
		t.Errorf("AdminID = %d, want 42", s.AdminID)
	}
	if s.Username != "root" {
		t.Errorf("Username = %q, want root", s.Username)
	}
	if s.CSRFToken() == "" {
		t.Error("CSRFToken() is empty")
	}
	if s.CSRFToken() != s.csrfToken {
		t.Error("CSRFToken() changed between calls")
	}

	got, ok := store.Get(s.ID)
	if !ok {
		t.Fatal("Get: not found")
	}
	if got.ID != s.ID {
		t.Errorf("Get ID = %q, want %q", got.ID, s.ID)
	}
}

func TestSessionStore_GetUnknown(t *testing.T) {
	store := NewSessionStore(time.Hour)
	if _, ok := store.Get("nonexistent"); ok {
		t.Error("Get unknown: want ok=false")
	}
}

func TestSessionStore_Destroy(t *testing.T) {
	store := NewSessionStore(time.Hour)
	s, _ := store.Create(1, "x")
	store.Destroy(s.ID)
	if _, ok := store.Get(s.ID); ok {
		t.Error("after Destroy: Get returned ok=true")
	}
}

func TestSessionStore_ExpiredNotReturned(t *testing.T) {
	store := NewSessionStore(10 * time.Millisecond)
	s, _ := store.Create(1, "x")
	time.Sleep(50 * time.Millisecond)
	if _, ok := store.Get(s.ID); ok {
		t.Error("expired session: Get returned ok=true")
	}
}

func TestSessionStore_IDsAreUnique(t *testing.T) {
	store := NewSessionStore(time.Hour)
	seen := map[string]bool{}
	for i := int64(0); i < 100; i++ {
		s, err := store.Create(i, "u")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if seen[s.ID] {
			t.Fatalf("duplicate session ID: %s", s.ID)
		}
		seen[s.ID] = true
	}
}

func TestSessionStore_GCRemovesExpired(t *testing.T) {
	store := NewSessionStore(10 * time.Millisecond)
	s, _ := store.Create(1, "x")
	time.Sleep(50 * time.Millisecond)
	store.GC()
	if _, ok := store.Get(s.ID); ok {
		t.Error("after GC: Get returned ok=true for expired session")
	}
}
