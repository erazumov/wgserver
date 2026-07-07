package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type Peer struct {
	ID                      int64
	Name                    string
	PublicKey               string
	PrivateKey              string
	PresharedKey            *string
	AssignedIP              string
	CreatedAt               int64
	ExpiresAt               *int64
	Disabled                bool
	PendingSync             bool
	CreatedByAdminID        *int64
	CreatedByTelegramUserID *int64
}

func CreatePeer(ctx context.Context, db *sql.DB, p Peer) (int64, error) {
	// New peers always start with pending_sync=1: the row exists before WG sees it.
	// WG failure keeps it that way so the retry loop can pick it up.
	res, err := db.ExecContext(ctx, `
		INSERT INTO peers (
			name, public_key, private_key, preshared_key, assigned_ip,
			created_at, expires_at, disabled, pending_sync,
			created_by_admin_id, created_by_telegram_user_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		p.Name, p.PublicKey, p.PrivateKey, p.PresharedKey, p.AssignedIP,
		p.CreatedAt, p.ExpiresAt, boolToInt(p.Disabled),
		p.CreatedByAdminID, p.CreatedByTelegramUserID,
	)
	if err != nil {
		return 0, fmt.Errorf("insert peer: %w", err)
	}
	return res.LastInsertId()
}

func GetPeerByID(ctx context.Context, db *sql.DB, id int64) (Peer, error) {
	return getPeer(ctx, db, `SELECT
		id, name, public_key, private_key, preshared_key, assigned_ip,
		created_at, expires_at, disabled, pending_sync,
		created_by_admin_id, created_by_telegram_user_id
		FROM peers WHERE id = ?`, id)
}

func GetPeerByPublicKey(ctx context.Context, db *sql.DB, pubkey string) (Peer, error) {
	return getPeer(ctx, db, `SELECT
		id, name, public_key, private_key, preshared_key, assigned_ip,
		created_at, expires_at, disabled, pending_sync,
		created_by_admin_id, created_by_telegram_user_id
		FROM peers WHERE public_key = ?`, pubkey)
}

func getPeer(ctx context.Context, db *sql.DB, query string, args ...any) (Peer, error) {
	var p Peer
	var presharedKey sql.NullString
	var expiresAt sql.NullInt64
	var disabled, pending int
	var adminID, tgID sql.NullInt64
	err := db.QueryRowContext(ctx, query, args...).Scan(
		&p.ID, &p.Name, &p.PublicKey, &p.PrivateKey, &presharedKey, &p.AssignedIP,
		&p.CreatedAt, &expiresAt, &disabled, &pending,
		&adminID, &tgID,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Peer{}, sql.ErrNoRows
		}
		return Peer{}, fmt.Errorf("select peer: %w", err)
	}
	if presharedKey.Valid {
		v := presharedKey.String
		p.PresharedKey = &v
	}
	if expiresAt.Valid {
		v := expiresAt.Int64
		p.ExpiresAt = &v
	}
	if adminID.Valid {
		v := adminID.Int64
		p.CreatedByAdminID = &v
	}
	if tgID.Valid {
		v := tgID.Int64
		p.CreatedByTelegramUserID = &v
	}
	p.Disabled = disabled != 0
	p.PendingSync = pending != 0
	return p, nil
}

func ListPeers(ctx context.Context, db *sql.DB) ([]Peer, error) {
	return listPeers(ctx, db, `SELECT
		id, name, public_key, private_key, preshared_key, assigned_ip,
		created_at, expires_at, disabled, pending_sync,
		created_by_admin_id, created_by_telegram_user_id
		FROM peers ORDER BY id`)
}

func ListPeersPendingSync(ctx context.Context, db *sql.DB) ([]Peer, error) {
	return listPeers(ctx, db, `SELECT
		id, name, public_key, private_key, preshared_key, assigned_ip,
		created_at, expires_at, disabled, pending_sync,
		created_by_admin_id, created_by_telegram_user_id
		FROM peers WHERE pending_sync = 1 ORDER BY id`)
}

// ListEnabledPeers returns every non-disabled peer regardless of
// pending_sync. Used by the syncer's reconciliation pass to detect
// divergence between DB and kernel state (e.g. after wg-quick@<iface>
// was restarted and the kernel peer list was wiped, but the DB still
// records pending_sync=0 because the syncer had nothing to retry).
func ListEnabledPeers(ctx context.Context, db *sql.DB) ([]Peer, error) {
	return listPeers(ctx, db, `SELECT
		id, name, public_key, private_key, preshared_key, assigned_ip,
		created_at, expires_at, disabled, pending_sync,
		created_by_admin_id, created_by_telegram_user_id
		FROM peers WHERE disabled = 0 ORDER BY id`)
}

func listPeers(ctx context.Context, db *sql.DB, query string) ([]Peer, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list peers: %w", err)
	}
	defer rows.Close()
	var out []Peer
	for rows.Next() {
		var p Peer
		var presharedKey sql.NullString
		var expiresAt sql.NullInt64
		var disabled, pending int
		var adminID, tgID sql.NullInt64
		if err := rows.Scan(
			&p.ID, &p.Name, &p.PublicKey, &p.PrivateKey, &presharedKey, &p.AssignedIP,
			&p.CreatedAt, &expiresAt, &disabled, &pending,
			&adminID, &tgID,
		); err != nil {
			return nil, fmt.Errorf("scan peer: %w", err)
		}
		if presharedKey.Valid {
			v := presharedKey.String
			p.PresharedKey = &v
		}
		if expiresAt.Valid {
			v := expiresAt.Int64
			p.ExpiresAt = &v
		}
		if adminID.Valid {
			v := adminID.Int64
			p.CreatedByAdminID = &v
		}
		if tgID.Valid {
			v := tgID.Int64
			p.CreatedByTelegramUserID = &v
		}
		p.Disabled = disabled != 0
		p.PendingSync = pending != 0
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows peers: %w", err)
	}
	return out, nil
}

func MarkPeerSynced(ctx context.Context, db *sql.DB, id int64) error {
	res, err := db.ExecContext(ctx, `UPDATE peers SET pending_sync = 0 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("mark peer synced: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func DisablePeer(ctx context.Context, db *sql.DB, id int64) error {
	res, err := db.ExecContext(ctx, `UPDATE peers SET disabled = 1, pending_sync = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("disable peer: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListAssignedIPs returns every (enabled or disabled) peer's assigned
// IP in CIDR form. Used by the IP allocator to skip used slots.
func ListAssignedIPs(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT assigned_ip FROM peers`)
	if err != nil {
		return nil, fmt.Errorf("list assigned_ips: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan assigned_ip: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows assigned_ips: %w", err)
	}
	return out, nil
}
