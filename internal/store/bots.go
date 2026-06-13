package store

import "context"

type Bot struct {
	UserID    string
	OwnerID   string
	TokenHash []byte
	CreatedAt int64
}

func (s *Store) CreateBot(ctx context.Context, b *Bot) error {
	b.CreatedAt = nowMS()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO bots (user_id, owner_id, token_hash, created_at) VALUES (?, ?, ?, ?)`,
		b.UserID, b.OwnerID, b.TokenHash, b.CreatedAt)
	return err
}

func (s *Store) GetBotByTokenHash(ctx context.Context, tokenHash []byte) (*Bot, error) {
	var b Bot
	err := s.db.QueryRowContext(ctx,
		`SELECT user_id, owner_id, token_hash, created_at FROM bots WHERE token_hash = ?`,
		tokenHash).Scan(&b.UserID, &b.OwnerID, &b.TokenHash, &b.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) ListBots(ctx context.Context) ([]*Bot, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, owner_id, token_hash, created_at FROM bots ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bots []*Bot
	for rows.Next() {
		var b Bot
		if err := rows.Scan(&b.UserID, &b.OwnerID, &b.TokenHash, &b.CreatedAt); err != nil {
			return nil, err
		}
		bots = append(bots, &b)
	}
	return bots, rows.Err()
}

func (s *Store) SetBotTokenHash(ctx context.Context, userID string, tokenHash []byte) error {
	_, err := s.db.ExecContext(ctx, `UPDATE bots SET token_hash = ? WHERE user_id = ?`, tokenHash, userID)
	return err
}
