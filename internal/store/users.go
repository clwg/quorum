package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type User struct {
	ID           string
	Username     string
	PasswordHash string // PHC string; empty for bot accounts
	Role         string // user | admin | bot
	Disabled     bool
	CreatedAt    int64 // unix ms
}

// NewID returns a 16-byte random hex identifier.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b[:])
}

func (s *Store) CreateUser(ctx context.Context, u *User) error {
	u.CreatedAt = nowMS()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, role, disabled, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		u.ID, u.Username, nullIfEmpty(u.PasswordHash), u.Role, boolToInt(u.Disabled), u.CreatedAt)
	return err
}

func (s *Store) GetUserByID(ctx context.Context, id string) (*User, error) {
	return s.scanUser(ctx, `SELECT id, username, COALESCE(password_hash,''), role, disabled, created_at FROM users WHERE id = ?`, id)
}

func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	return s.scanUser(ctx, `SELECT id, username, COALESCE(password_hash,''), role, disabled, created_at FROM users WHERE username = ?`, username)
}

func (s *Store) scanUser(ctx context.Context, query string, args ...any) (*User, error) {
	var u User
	var disabled int
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &disabled, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	u.Disabled = disabled != 0
	return &u, nil
}

func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, COALESCE(password_hash,''), role, disabled, created_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []*User
	for rows.Next() {
		var u User
		var disabled int
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &disabled, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.Disabled = disabled != 0
		users = append(users, &u)
	}
	return users, rows.Err()
}

func (s *Store) SetUserRole(ctx context.Context, id, role string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, id)
	return err
}

func (s *Store) SetUserDisabled(ctx context.Context, id string, disabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET disabled = ? WHERE id = ?`, boolToInt(disabled), id)
	return err
}

func (s *Store) SetUserPassword(ctx context.Context, id, phc string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, phc, id)
	return err
}

func (s *Store) DeleteUser(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	return err
}

func (s *Store) SetIdentityKey(ctx context.Context, userID string, publicKey []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO identity_keys (user_id, public_key, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET public_key = excluded.public_key, updated_at = excluded.updated_at`,
		userID, publicKey, nowMS())
	return err
}

func (s *Store) GetIdentityKey(ctx context.Context, userID string) ([]byte, error) {
	var key []byte
	err := s.db.QueryRowContext(ctx, `SELECT public_key FROM identity_keys WHERE user_id = ?`, userID).Scan(&key)
	if err != nil {
		return nil, err
	}
	return key, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
