package store

import (
	"context"
	"time"
)

type Session struct {
	TokenHash []byte
	UserID    string
	CreatedAt int64
	ExpiresAt int64
	LastSeen  int64
}

func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, userID string, ttl time.Duration) (*Session, error) {
	now := nowMS()
	sess := &Session{
		TokenHash: tokenHash,
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now + ttl.Milliseconds(),
		LastSeen:  now,
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_seen) VALUES (?, ?, ?, ?, ?)`,
		sess.TokenHash, sess.UserID, sess.CreatedAt, sess.ExpiresAt, sess.LastSeen)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

func (s *Store) GetSession(ctx context.Context, tokenHash []byte) (*Session, error) {
	var sess Session
	err := s.db.QueryRowContext(ctx,
		`SELECT token_hash, user_id, created_at, expires_at, last_seen FROM sessions WHERE token_hash = ?`,
		tokenHash).Scan(&sess.TokenHash, &sess.UserID, &sess.CreatedAt, &sess.ExpiresAt, &sess.LastSeen)
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

// TouchSession extends a session's sliding expiry window.
func (s *Store) TouchSession(ctx context.Context, tokenHash []byte, ttl time.Duration) error {
	now := nowMS()
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET last_seen = ?, expires_at = ? WHERE token_hash = ?`,
		now, now+ttl.Milliseconds(), tokenHash)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, nowMS())
	return err
}
