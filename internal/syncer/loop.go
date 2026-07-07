package syncer

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/erazumov/wgserver/internal/store"
	"github.com/erazumov/wgserver/internal/wg"
)

// Runner is the subset of wg.Runner the syncer needs. Tests pass a
// fake; production uses wg.ExecRunner. We re-use the wg.Runner
// interface so the same ExecRunner / fake satisfies both packages.
type Runner = wg.Runner

type Loop struct {
	DB        *sql.DB
	Runner    Runner
	Interface string
	// PSKDir is the directory the syncer writes per-peer preshared-key
	// files into. wireguard-tools 1.0.20210914 (Debian 12) requires
	// the `preshared-key` value of `wg set` to be a file path; the syncer
	// writes the PSK here and passes the path. Must be writable by the
	// wgserver user.
	PSKDir   string
	Logger   *log.Logger
	Interval time.Duration
}

// RunOnce reconciles DB state with the WireGuard kernel state in
// two passes:
//
//  1. Pending pass: pull every peer with pending_sync=1 from the DB
//     and apply it (add or remove). On success mark the row synced;
//     on failure leave it pending and continue with the next peer.
//     This is the original reactive pass — admin actions and Telegram
//     bot claims land here.
//
//  2. Reconcile pass: pull the current peer set from the kernel via
//     `wg show <iface> peers` and compare it to the set of non-
//     disabled peers in the DB. For any DB peer that the kernel has
//     lost, re-apply it. This catches divergence that the pending
//     pass cannot: if wg-quick@<iface> was restarted (by deploy.sh
//     upgrade, manual `systemctl restart wg-quick@wg0`, or an
//     operator's `wg syncconf`), the kernel peer list is wiped but
//     the DB still records pending_sync=0 for every peer — so the
//     pending pass has nothing to do. Without reconciliation, those
//     peers stay unreachable on the kernel until the next admin
//     action sets pending_sync=1 again. See AGENTS.md invariant
//     "WireGuard state changes are not transactional with the DB".
//
// Per-peer failures in either pass are logged and skipped — never
// silently desync. RunOnce never returns an error from per-peer
// failures; it only returns non-nil if the DB read itself fails.
func (l *Loop) RunOnce(ctx context.Context) error {
	pending, err := store.ListPeersPendingSync(ctx, l.DB)
	if err != nil {
		return fmt.Errorf("list pending: %w", err)
	}
	for _, p := range pending {
		if err := l.applyOne(ctx, p); err != nil {
			l.Logger.Printf("syncer: peer %d (%s) %v", p.ID, p.PublicKey, err)
			continue
		}
	}
	if err := l.reconcile(ctx); err != nil {
		// Reconciliation failures are not fatal: the next tick will
		// retry, and pending apply above is unaffected. Log and move on.
		l.Logger.Printf("syncer: reconcile: %v", err)
	}
	return nil
}

func (l *Loop) applyOne(ctx context.Context, p store.Peer) error {
	var err error
	if p.Disabled {
		err = wg.RemovePeer(l.Runner, l.Interface, l.PSKDir, p.PublicKey)
	} else {
		psk := ""
		if p.PresharedKey != nil {
			psk = *p.PresharedKey
		}
		err = wg.AddPeer(l.Runner, l.Interface, p.PublicKey, p.AssignedIP, l.PSKDir, psk)
	}
	if err != nil {
		return err
	}
	if err := store.MarkPeerSynced(ctx, l.DB, p.ID); err != nil {
		return fmt.Errorf("mark synced: %w", err)
	}
	return nil
}

// reconcile re-applies any enabled DB peer that the kernel has lost.
// See RunOnce for the rationale. Errors fetching either side are
// returned to the caller (which logs and continues); per-peer apply
// errors are logged and skipped so one bad row does not block the rest.
func (l *Loop) reconcile(ctx context.Context) error {
	kernelPeers, err := l.currentWGPeers(ctx)
	if err != nil {
		return err
	}
	dbPeers, err := store.ListEnabledPeers(ctx, l.DB)
	if err != nil {
		return fmt.Errorf("list enabled: %w", err)
	}
	for _, p := range dbPeers {
		if _, ok := kernelPeers[p.PublicKey]; ok {
			continue
		}
		// The peer exists in the DB but the kernel has lost it
		// (typically because wg-quick was restarted and the empty
		// wg0.conf was reloaded). Re-apply so the tunnel comes back.
		// applyOne marks the row synced on success, but a divergence
		// re-detection on the next tick is cheap and correct.
		if err := l.applyOne(ctx, p); err != nil {
			l.Logger.Printf("syncer: reconcile peer %d (%s) %v", p.ID, p.PublicKey, err)
			continue
		}
		l.Logger.Printf("syncer: reconcile re-added peer %d (%s) (was missing from kernel)", p.ID, p.PublicKey)
	}
	return nil
}

// currentWGPeers parses `wg show <iface> peers` output (one base64
// public key per line) into a set for O(1) membership checks in the
// reconciliation pass. An empty output (no peers configured, or
// `wg show` returned an empty string) is a valid result, not an error.
func (l *Loop) currentWGPeers(ctx context.Context) (map[string]struct{}, error) {
	out, err := l.Runner.Output("wg", "show", l.Interface, "peers")
	if err != nil {
		return nil, fmt.Errorf("wg show %s peers: %w", l.Interface, err)
	}
	set := make(map[string]struct{})
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		set[line] = struct{}{}
	}
	return set, nil
}

// Run drives the loop until ctx is cancelled. One immediate RunOnce is
// performed before the first tick so that any rows that accumulated
// while the server was down get picked up promptly.
func (l *Loop) Run(ctx context.Context) {
	if err := l.RunOnce(ctx); err != nil {
		l.Logger.Printf("syncer: initial run: %v", err)
	}
	if l.Interval <= 0 {
		<-ctx.Done()
		return
	}
	t := time.NewTicker(l.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.RunOnce(ctx); err != nil {
				l.Logger.Printf("syncer: tick: %v", err)
			}
		}
	}
}
