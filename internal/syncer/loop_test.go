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
	mu      sync.Mutex
	calls   []string
	err     error
	wgPeers map[string]struct{} // current peer set in the fake wg0
}

func (f *fakeRunner) Run(name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if err := f.err; err != nil {
		return err
	}
	// Track `wg set wg0 peer <pubkey> [allowed-ips X | remove]` so
	// reconciliation tests can read the current state back via
	// `wg show wg0 peers` (Output). Anything else is ignored.
	if name == "wg" && len(args) >= 2 && args[0] == "set" {
		if f.wgPeers == nil {
			f.wgPeers = map[string]struct{}{}
		}
		for i := 0; i < len(args); i++ {
			if args[i] != "peer" || i+1 >= len(args) {
				continue
			}
			pubkey := args[i+1]
			if i+2 < len(args) && args[i+2] == "remove" {
				delete(f.wgPeers, pubkey)
			} else {
				f.wgPeers[pubkey] = struct{}{}
			}
		}
	}
	return nil
}

func (f *fakeRunner) Output(name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name == "wg" && len(args) >= 3 && args[0] == "show" && args[2] == "peers" {
		if len(f.wgPeers) == 0 {
			return "", nil
		}
		out := make([]string, 0, len(f.wgPeers))
		for k := range f.wgPeers {
			out = append(out, k)
		}
		return strings.Join(out, "\n"), nil
	}
	return "", nil
}

func (f *fakeRunner) OutputStdin(name string, args []string, _ io.Reader) (string, error) {
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
	return c.fake.Output(name, args...)
}

func (c *countingRunner) OutputStdin(name string, args []string, _ io.Reader) (string, error) {
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
		Interface: "wg0",
		PSKDir:    t.TempDir(),
		Logger:    log.New(io.Discard, "", 0),
		Interval:  time.Hour,
	}
}

type runner interface {
	Run(name string, args ...string) error
	Output(name string, args ...string) (string, error)
	OutputStdin(name string, args []string, _ io.Reader) (string, error)
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
	want := "wg set wg0 peer PUBKEY_A allowed-ips 10.0.1.2/32"
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
	if got := r.callsCopy(); len(got) != 1 || got[0] != "wg set wg0 peer PUBKEY_A allowed-ips 10.0.1.2/32" {
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
	if calls[1] != "wg set wg0 peer PUBKEY_A remove" {
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
	const wantPrefix = "wg set wg0 peer PUBKEY_A allowed-ips 10.0.1.2/32 preshared-key "
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
	// RunOnce now does two passes that each try to add the peer:
	// the original pending pass AND the new reconcile pass (which
	// sees the kernel has no peer and retries). Both fail because
	// the runner always errors, so 2 recorded calls and the row
	// stays pending.
	if calls := r.callsCopy(); len(calls) != 2 {
		t.Fatalf("calls = %d, want 2 (pending + reconcile retries)", len(calls))
	}
	pending, _ := peerState(t, db, id)
	if !pending {
		t.Error("peer marked synced despite wg error; invariant violated (must retry)")
	}
}

func TestRunOnce_ContinuesOnError(t *testing.T) {
	db := newTestDB(t)
	// failN=3: the pending pass and the reconcile pass both fail for
	// peer A, so the row stays pending. B's reconcile attempt is the
	// fourth call (n=4 > failN=3) and succeeds — recorded exactly
	// once, demonstrating that "one peer failing does not block the
	// rest" survives the reconciliation pass.
	r := &countingRunner{failN: 3}
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
	calls := r.callsCopy()
	if len(calls) != 1 {
		t.Fatalf("recorded successful calls = %v, want exactly 1 (only B's reconcile)", calls)
	}
	if calls[0] != "wg set wg0 peer PUBKEY_B allowed-ips 10.0.1.3/32" {
		t.Errorf("success call = %q, want wg set wg0 peer PUBKEY_B ...", calls[0])
	}
	pendingA, _ := peerState(t, db, idA)
	pendingB, _ := peerState(t, db, idB)
	if !pendingA {
		t.Error("A: should still be pending (both pending and reconcile attempts failed)")
	}
	if pendingB {
		t.Error("B: should be marked synced (B's reconcile attempt succeeded)")
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

// TestRunOnce_ReconcileAfterWGWipe reproduces the production bug:
// wg-quick@<iface> was restarted (e.g. by deploy.sh on upgrade,
// manual `systemctl restart wg-quick@wg0`, or any `wg syncconf`),
// the kernel peer list was wiped, but the DB still records
// pending_sync=0 for every peer because the syncer had nothing to
// retry. Without the reconciliation pass added to RunOnce, those
// peers stay unreachable on the kernel until the next admin action
// sets pending_sync=1 again.
func TestRunOnce_ReconcileAfterWGWipe(t *testing.T) {
	db := newTestDB(t)
	r := &fakeRunner{}
	l := newTestLoop(t, db, r)

	id := seedPeer(t, db, store.Peer{
		Name: "alice", PublicKey: "PUBKEY_A", PrivateKey: "PRIV_A",
		AssignedIP: "10.0.1.2/32", CreatedAt: time.Now().Unix(),
	})

	// First tick: peer is pending, gets added to the kernel.
	if err := l.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 1: %v", err)
	}
	if got := r.callsCopy(); len(got) != 1 {
		t.Fatalf("first tick: calls = %v, want 1", got)
	}
	if _, ok := r.wgPeers["PUBKEY_A"]; !ok {
		t.Fatalf("after first tick, peer A should be in fake wg0, got %v", r.wgPeers)
	}

	// Simulate the kernel wiping the peer list (wg-quick restart).
	// The DB row stays put with pending_sync=0 because the syncer
	// had previously marked it synced.
	r.wgPeers = map[string]struct{}{}

	// Second tick: pending pass has nothing to do (peer is
	// pending_sync=0). Reconciliation must detect the divergence
	// and re-apply the peer.
	if err := l.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 2: %v", err)
	}
	calls := r.callsCopy()
	if len(calls) != 2 {
		t.Fatalf("second tick: calls = %v, want 2 (initial add + reconcile re-add)", calls)
	}
	if calls[1] != "wg set wg0 peer PUBKEY_A allowed-ips 10.0.1.2/32" {
		t.Errorf("reconcile call = %q, want wg set wg0 peer PUBKEY_A ...", calls[1])
	}
	if _, ok := r.wgPeers["PUBKEY_A"]; !ok {
		t.Errorf("after reconcile, peer A should be back in fake wg0, got %v", r.wgPeers)
	}
	pending, _ := peerState(t, db, id)
	if pending {
		t.Error("peer re-applied successfully should be marked synced, not pending")
	}
}

// TestRunOnce_ReconcileSkipsDisabledPeers: reconciliation must not
// re-add a peer that the admin has disabled, even if the kernel has
// lost it. Disabled peers live in the DB only as tombstones.
func TestRunOnce_ReconcileSkipsDisabledPeers(t *testing.T) {
	db := newTestDB(t)
	r := &fakeRunner{}
	l := newTestLoop(t, db, r)

	id := seedPeer(t, db, store.Peer{
		Name: "alice", PublicKey: "PUBKEY_A", PrivateKey: "PRIV_A",
		AssignedIP: "10.0.1.2/32", CreatedAt: time.Now().Unix(),
	})
	// Add the peer to the kernel.
	if err := l.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 1: %v", err)
	}
	// Admin revokes: disable + pending_sync=1, then the pending
	// pass removes the peer from the kernel.
	if err := store.DisablePeer(context.Background(), db, id); err != nil {
		t.Fatalf("DisablePeer: %v", err)
	}
	if err := l.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 2: %v", err)
	}
	if _, ok := r.wgPeers["PUBKEY_A"]; ok {
		t.Fatalf("after disable, peer A should be removed from wg0, got %v", r.wgPeers)
	}
	// Third tick: reconciliation sees no enabled DB peer for A, so
	// it must not re-add the tombstoned peer. Only 2 calls total
	// (the original add and the removal) — no third call.
	if err := l.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce 3: %v", err)
	}
	if got := r.callsCopy(); len(got) != 2 {
		t.Errorf("calls = %v, want 2 (add + remove, no reconcile re-add of disabled peer)", got)
	}
	if _, ok := r.wgPeers["PUBKEY_A"]; ok {
		t.Errorf("disabled peer must not reappear in wg0, got %v", r.wgPeers)
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
