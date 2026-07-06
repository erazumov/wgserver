package syncer

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erazumov/wgserver/internal/store"
)

type fakeRunner struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeRunner) Run(name string, args ...string) error {
	f.mu.Lock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	err := f.err
	f.mu.Unlock()
	return err
}

func (f *fakeRunner) Output(name string, args ...string) (string, error) {
	return "", nil
}

func (f *fakeRunner) callsCopy() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// countingRunner fails the first failN Run() calls, then delegates to
// fakeRunner. Lets us test that a failing peer does not block subsequent
// peers in the same tick.
type countingRunner struct {
	fake  fakeRunner
	failN int
	mu    sync.Mutex
	n     int
}

func (c *countingRunner) Run(name string, args ...string) error {
	c.mu.Lock()
	c.n++
	n := c.n
	c.mu.Unlock()
	if n <= c.failN {
		return errors.New("simulated wg failure")
	}
	return c.fake.Run(name, args...)
}

func (c *countingRunner) Output(name string, args ...string) (string, error) {
	return "", nil
}

func (c *countingRunner) callsCopy() []string {
	return c.fake.callsCopy()
}

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "wgserver.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newTestLoop(t *testing.T, db *sql.DB, r runner) *Loop {
	t.Helper()
	return &Loop{
		DB:        db,
		Runner:    r,
		Interface: "wg1",
		PSKDir:    t.TempDir(),
		Logger:    log.New(io.Discard, "", 0),
		Interval:  time.Hour,
	}
}

type runner interface {
	Run(name string, args ...string) error
	Output(name string, args ...string) (string, error)
}

func seedPeer(t *testing.T, db *sql.DB, p store.Peer) int64 {
	t.Helper()
	id, err := store.CreatePeer(context.Background(), db, p)
	if err != nil {
		t.Fatalf("CreatePeer: %v", err)
	}
	return id
}

func peerState(t *testing.T, db *sql.DB, id int64) (pending bool, disabled bool) {
	t.Helper()
	p, err := store.GetPeerByID(context.Background(), db, id)
	if err != nil {
		t.Fatalf("GetPeerByID: %v", err)
	}
	return p.PendingSync, p.Disabled
}

func TestRunOnce_EmptyNoCalls(t *testing.T) {
	db := newTestDB(t)
	r := &fakeRunner{}
	l := newTestLoop(t, db, r)
	if err := l.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := r.callsCopy(); len(got) != 0 {
		t.Errorf("calls = %v, want none", got)
	}
}

func TestRunOnce_AddsNewPeer(t *testing.T) {
	db := newTestDB(t)
	r := &fakeRunner{}
	l := newTestLoop(t, db, r)

	id := seedPeer(t, db, store.Peer{
		Name: "alice", PublicKey: "PUBKEY_A", PrivateKey: "PRIV_A",
		AssignedIP: "10.0.1.2/32", CreatedAt: time.Now().Unix(),
	})

	if err := l.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	calls := r.callsCopy()
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want 1", calls)
	}
	want := "wg set wg1 peer PUBKEY_A allowed-ips 10.0.1.2/32"
	if calls[0] != want {
		t.Errorf("call = %q, want %q", calls[0], want)
	}

	pending, disabled := peerState(t, db, id)
	if pending {
		t.Error("peer still pending_sync=1 after successful add")
	}
	if disabled {
		t.Error("peer disabled after add")
	}
}

func TestRunOnce_RemovesDisabledPeer(t *testing.T) {
	db := newTestDB(t)
	r := &fakeRunner{}
	l := newTestLoop(t, db, r)

	id := seedPeer(t, db, store.Peer{
		Name: "alice", PublicKey: "PUBKEY_A", PrivateKey: "PRIV_A",
		AssignedIP: "10.0.1.2/32", CreatedAt: time.Now().Unix(),
	})

	// First sync: peer is new and active, loop should add it.
	if err := l.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 1 (add): %v", err)
	}
	if got := r.callsCopy(); len(got) != 1 || got[0] != "wg set wg1 peer PUBKEY_A allowed-ips 10.0.1.2/32" {
		t.Fatalf("first call = %v, want add", got)
	}

	// Now disable (admin revokes). DisablePeer re-sets pending_sync=1.
	if err := store.DisablePeer(context.Background(), db, id); err != nil {
		t.Fatalf("DisablePeer: %v", err)
	}

	// Second sync: peer is now disabled+pending, loop should remove it.
	if err := l.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 2 (remove): %v", err)
	}
	calls := r.callsCopy()
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want 2", calls)
	}
	if calls[1] != "wg set wg1 peer PUBKEY_A remove" {
		t.Errorf("second call = %q, want remove", calls[1])
	}
	pending, _ := peerState(t, db, id)
	if pending {
		t.Error("peer still pending_sync=1 after successful remove")
	}
}

func TestRunOnce_PreservesPresharedKey(t *testing.T) {
	db := newTestDB(t)
	r := &fakeRunner{}
	l := newTestLoop(t, db, r)

	psk := "PSK_BASE64"
	seedPeer(t, db, store.Peer{
		Name: "alice", PublicKey: "PUBKEY_A", PrivateKey: "PRIV_A",
		PresharedKey: &psk,
		AssignedIP:   "10.0.1.2/32", CreatedAt: time.Now().Unix(),
	})
	if err := l.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	calls := r.callsCopy()
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want 1", calls)
	}
	// The wireguard-tools 1.0.20210914 `wg set preshared-key` only
	// accepts a file path, so the syncer must have written the PSK to
	// a file and passed that path. Verify the arg shape and that the
	// file exists with the PSK content.
	const wantPrefix = "wg set wg1 peer PUBKEY_A allowed-ips 10.0.1.2/32 preshared-key "
	if !strings.HasPrefix(calls[0], wantPrefix) {
		t.Errorf("call = %q, want prefix %q", calls[0], wantPrefix)
	}
	pskPath := strings.TrimPrefix(calls[0], wantPrefix)
	body, err := os.ReadFile(pskPath)
	if err != nil {
		t.Fatalf("read psk file %q: %v", pskPath, err)
	}
	if got := strings.TrimRight(string(body), "\n"); got != psk {
		t.Errorf("psk file content = %q, want %q", got, psk)
	}
}

func TestRunOnce_OnErrorPeerStaysPending(t *testing.T) {
	db := newTestDB(t)
	r := &fakeRunner{err: errors.New("wg set failed")}
	l := newTestLoop(t, db, r)

	id := seedPeer(t, db, store.Peer{
		Name: "alice", PublicKey: "PUBKEY_A", PrivateKey: "PRIV_A",
		AssignedIP: "10.0.1.2/32", CreatedAt: time.Now().Unix(),
	})
	if err := l.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if calls := r.callsCopy(); len(calls) != 1 {
		t.Fatalf("calls = %v, want 1", calls)
	}
	pending, _ := peerState(t, db, id)
	if !pending {
		t.Error("peer marked synced despite wg error; invariant violated (must retry)")
	}
}

func TestRunOnce_ContinuesOnError(t *testing.T) {
	db := newTestDB(t)
	r := &countingRunner{failN: 1}
	l := newTestLoop(t, db, r)

	idA := seedPeer(t, db, store.Peer{
		Name: "a", PublicKey: "PUBKEY_A", PrivateKey: "PVA",
		AssignedIP: "10.0.1.2/32", CreatedAt: time.Now().Unix(),
	})
	idB := seedPeer(t, db, store.Peer{
		Name: "b", PublicKey: "PUBKEY_B", PrivateKey: "PVB",
		AssignedIP: "10.0.1.3/32", CreatedAt: time.Now().Unix(),
	})

	if err := l.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// countingRunner's failN=1 means the first call (A) fails, the
	// second (B) goes through to fakeRunner which records it.
	if calls := r.callsCopy(); len(calls) != 1 {
		t.Errorf("recorded successful calls = %d, want 1 (only B)", len(calls))
	}
	if calls := r.callsCopy(); len(calls) > 0 && calls[0] != "wg set wg1 peer PUBKEY_B allowed-ips 10.0.1.3/32" {
		t.Errorf("success call = %q, want wg set wg1 peer PUBKEY_B ...", calls[0])
	}
	pendingA, _ := peerState(t, db, idA)
	pendingB, _ := peerState(t, db, idB)
	if !pendingA {
		t.Error("A: should still be pending after failure")
	}
	if pendingB {
		t.Error("B: should be marked synced (B succeeded)")
	}
}

func TestRunOnce_AllPending(t *testing.T) {
	db := newTestDB(t)
	r := &fakeRunner{}
	l := newTestLoop(t, db, r)

	for i := 1; i <= 3; i++ {
		seedPeer(t, db, store.Peer{
			Name:       "p",
			PublicKey:  "PK" + string(rune('0'+i)),
			PrivateKey: "PV",
			AssignedIP: "10.0.1." + string(rune('1'+i)) + "/32",
			CreatedAt:  time.Now().Unix(),
		})
	}
	if err := l.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if calls := r.callsCopy(); len(calls) != 3 {
		t.Errorf("calls = %d, want 3", len(calls))
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	db := newTestDB(t)
	r := &fakeRunner{}
	l := newTestLoop(t, db, r)
	l.Interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		l.Run(ctx)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s of cancel")
	}
}
