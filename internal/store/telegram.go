package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrQuotaExceeded = errors.New("telegram user quota exceeded")

type TelegramUser struct {
	ID         int64
	Username   string
	FirstName  string
	QuotaUsed  int
	LastSeenAt int64
	IsMember   bool
}

func UpsertTelegramUser(ctx context.Context, db *sql.DB, u TelegramUser) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO telegram_users (telegram_user_id, username, first_name, quota_used, last_seen_at, is_member)
		VALUES (?, ?, ?, 0, ?, ?)
		ON CONFLICT(telegram_user_id) DO UPDATE SET
			username    = excluded.username,
			first_name  = excluded.first_name,
			last_seen_at = excluded.last_seen_at,
			is_member    = excluded.is_member`,
		u.ID, nullString(u.Username), nullString(u.FirstName), u.LastSeenAt, boolToInt(u.IsMember),
	)
	if err != nil {
		return fmt.Errorf("upsert telegram user: %w", err)
	}
	return nil
}

func GetTelegramUser(ctx context.Context, db *sql.DB, id int64) (TelegramUser, error) {
	var u TelegramUser
	var username, firstName sql.NullString
	var isMember int
	err := db.QueryRowContext(ctx, `
		SELECT telegram_user_id, username, first_name, quota_used, last_seen_at, is_member
		FROM telegram_users WHERE telegram_user_id = ?`, id,
	).Scan(&u.ID, &username, &firstName, &u.QuotaUsed, &u.LastSeenAt, &isMember)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TelegramUser{}, sql.ErrNoRows
		}
		return TelegramUser{}, fmt.Errorf("get telegram user: %w", err)
	}
	if username.Valid {
		u.Username = username.String
	}
	if firstName.Valid {
		u.FirstName = firstName.String
	}
	u.IsMember = isMember != 0
	return u, nil
}

// ClaimPeerForTelegramUser is the ONLY supported way to grant a peer to a
// Telegram user. It enforces the per-user quota inside a single SQL
// transaction, so a second bot instance or a direct DB write cannot
// bypass it.
func ClaimPeerForTelegramUser(ctx context.Context, db *sql.DB, tgUserID int64, p Peer, limit int) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var used int
	err = tx.QueryRowContext(ctx,
		`SELECT quota_used FROM telegram_users WHERE telegram_user_id = ?`, tgUserID,
	).Scan(&used)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, sql.ErrNoRows
		}
		return 0, fmt.Errorf("read quota: %w", err)
	}
	if used >= limit {
		return 0, ErrQuotaExceeded
	}

	peer := p
	tgID := tgUserID
	peer.CreatedByTelegramUserID = &tgID
	peer.CreatedByAdminID = nil

	res, err := tx.ExecContext(ctx, `
		INSERT INTO peers (
			name, public_key, private_key, preshared_key, assigned_ip,
			created_at, expires_at, disabled, pending_sync,
			created_by_admin_id, created_by_telegram_user_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, 0, 1, NULL, ?)`,
		peer.Name, peer.PublicKey, peer.PrivateKey, peer.PresharedKey, peer.AssignedIP,
		peer.CreatedAt, peer.ExpiresAt, peer.CreatedByTelegramUserID,
	)
	if err != nil {
		return 0, fmt.Errorf("insert peer: %w", err)
	}
	peerID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("peer last id: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO telegram_claims (telegram_user_id, peer_id, claimed_at) VALUES (?, ?, ?)`,
		tgUserID, peerID, peer.CreatedAt,
	); err != nil {
		return 0, fmt.Errorf("insert claim: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE telegram_users SET quota_used = quota_used + 1 WHERE telegram_user_id = ?`,
		tgUserID,
	); err != nil {
		return 0, fmt.Errorf("increment quota: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit claim: %w", err)
	}
	return peerID, nil
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
