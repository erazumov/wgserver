package syncer

import (
	"context"
	"database/sql"
	"fmt"
	"log"
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

// RunOnce pulls every peer with pending_sync=1 and reconciles it with
// the WireGuard interface. On success the row is marked synced; on
// failure the row stays pending and the next tick will retry — never
// silently desync. See AGENTS.md invariant.
//
// RunOnce never returns an error: per-peer failures are logged and the
// loop continues with the next peer. The return value exists so the
// caller can distinguish "DB read failed" (return non-nil) from "all
// peers processed (some may have failed at the wg layer)".
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
