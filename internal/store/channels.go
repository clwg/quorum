package store

import "context"

type Channel struct {
	ID        string
	Name      string
	Topic     string
	CreatedBy string
	CreatedAt int64
	IsMember  bool // populated by ListChannelsForUser
}

type Message struct {
	ID         int64
	ChannelID  string
	SenderID   string
	SenderName string
	Body       string
	CreatedAt  int64
}

func (s *Store) CreateChannel(ctx context.Context, c *Channel) error {
	c.CreatedAt = nowMS()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO channels (id, name, topic, created_by, created_at) VALUES (?, ?, ?, ?, ?)`,
		c.ID, c.Name, c.Topic, c.CreatedBy, c.CreatedAt)
	return err
}

func (s *Store) GetChannel(ctx context.Context, id string) (*Channel, error) {
	var c Channel
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, topic, COALESCE(created_by,''), created_at FROM channels WHERE id = ?`,
		id).Scan(&c.ID, &c.Name, &c.Topic, &c.CreatedBy, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) GetChannelByName(ctx context.Context, name string) (*Channel, error) {
	var c Channel
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, topic, COALESCE(created_by,''), created_at FROM channels WHERE name = ?`,
		name).Scan(&c.ID, &c.Name, &c.Topic, &c.CreatedBy, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListChannelsForUser returns all channels with IsMember set for the given user.
func (s *Store) ListChannelsForUser(ctx context.Context, userID string) ([]*Channel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.name, c.topic, COALESCE(c.created_by,''), c.created_at,
		        EXISTS(SELECT 1 FROM channel_members m WHERE m.channel_id = c.id AND m.user_id = ?)
		 FROM channels c ORDER BY c.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chans []*Channel
	for rows.Next() {
		var c Channel
		var isMember int
		if err := rows.Scan(&c.ID, &c.Name, &c.Topic, &c.CreatedBy, &c.CreatedAt, &isMember); err != nil {
			return nil, err
		}
		c.IsMember = isMember != 0
		chans = append(chans, &c)
	}
	return chans, rows.Err()
}

func (s *Store) AddChannelMember(ctx context.Context, channelID, userID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO channel_members (channel_id, user_id, joined_at) VALUES (?, ?, ?)`,
		channelID, userID, nowMS())
	return err
}

func (s *Store) RemoveChannelMember(ctx context.Context, channelID, userID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM channel_members WHERE channel_id = ? AND user_id = ?`, channelID, userID)
	return err
}

func (s *Store) IsChannelMember(ctx context.Context, channelID, userID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM channel_members WHERE channel_id = ? AND user_id = ?`,
		channelID, userID).Scan(&n)
	return n > 0, err
}

func (s *Store) ChannelMemberIDs(ctx context.Context, channelID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id FROM channel_members WHERE channel_id = ?`, channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) InsertMessage(ctx context.Context, m *Message) error {
	m.CreatedAt = nowMS()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages (channel_id, sender_id, body, created_at) VALUES (?, ?, ?, ?)`,
		m.ChannelID, m.SenderID, m.Body, m.CreatedAt)
	if err != nil {
		return err
	}
	m.ID, err = res.LastInsertId()
	return err
}

// ChannelHistory returns up to limit messages with id < beforeID (or the
// latest if beforeID <= 0), in ascending id order.
func (s *Store) ChannelHistory(ctx context.Context, channelID string, beforeID int64, limit int) ([]*Message, error) {
	if beforeID <= 0 {
		beforeID = int64(1)<<62 - 1
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id, m.channel_id, m.sender_id, u.username, m.body, m.created_at
		 FROM messages m JOIN users u ON u.id = m.sender_id
		 WHERE m.channel_id = ? AND m.id < ?
		 ORDER BY m.id DESC LIMIT ?`, channelID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []*Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.SenderID, &m.SenderName, &m.Body, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, &m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// reverse to ascending
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}
