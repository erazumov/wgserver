package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type Admin struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    int64
	Disabled     bool
}

func CreateAdmin(ctx context.Context, db *sql.DB, a Admin) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO admins (username, password_hash, created_at, disabled) VALUES (?, ?, ?, ?)`,
		a.Username, a.PasswordHash, a.CreatedAt, boolToInt(a.Disabled),
	)
	if err != nil {
		return 0, fmt.Errorf("insert admin: %w", err)
	}
	return res.LastInsertId()
}

func GetAdminByUsername(ctx context.Context, db *sql.DB, username string) (Admin, error) {
	var a Admin
	var disabled int
	err := db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at, disabled FROM admins WHERE username = ?`,
		username,
	).Scan(&a.ID, &a.Username, &a.PasswordHash, &a.CreatedAt, &disabled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Admin{}, sql.ErrNoRows
		}
		return Admin{}, fmt.Errorf("select admin: %w", err)
	}
	a.Disabled = disabled != 0
	return a, nil
}

func ListAdmins(ctx context.Context, db *sql.DB) ([]Admin, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, username, password_hash, created_at, disabled FROM admins ORDER BY id`,
	)
	if err != nil {
		return nil, fmt.Errorf("list admins: %w", err)
	}
	defer rows.Close()
	var out []Admin
	for rows.Next() {
		var a Admin
		var disabled int
		if err := rows.Scan(&a.ID, &a.Username, &a.PasswordHash, &a.CreatedAt, &disabled); err != nil {
			return nil, fmt.Errorf("scan admin: %w", err)
		}
		a.Disabled = disabled != 0
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows admins: %w", err)
	}
	return out, nil
}

func DisableAdmin(ctx context.Context, db *sql.DB, id int64) error {
	res, err := db.ExecContext(ctx, `UPDATE admins SET disabled = 1 WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("disable admin: %w", err)
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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
