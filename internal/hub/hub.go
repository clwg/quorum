// Package hub routes realtime events to connected subscribers and tracks
// presence. Presence is derived purely from the subscriber map; nothing
// here touches the database.
package hub

import (
	"sync"

	quorumv1 "github.com/layer8/quorum/gen/quorum/v1"
)

const subscriberBuffer = 256

// Subscriber is one user's active event stream. A user has at most one;
// a newer subscription replaces (kicks) the previous one.
type Subscriber struct {
	UserID   string
	Username string
	Ch       chan *quorumv1.ServerEvent
	Done     chan struct{} // closed to force the stream to end

	closeOnce sync.Once
}

// Close signals the owning stream handler to terminate.
func (s *Subscriber) Close() {
	s.closeOnce.Do(func() { close(s.Done) })
}

type Hub struct {
	mu   sync.RWMutex
	subs map[string]*Subscriber
}

func New() *Hub {
	return &Hub{subs: make(map[string]*Subscriber)}
}

// Register adds a subscriber for the user, kicking any previous one.
func (h *Hub) Register(userID, username string) *Subscriber {
	sub := &Subscriber{
		UserID:   userID,
		Username: username,
		Ch:       make(chan *quorumv1.ServerEvent, subscriberBuffer),
		Done:     make(chan struct{}),
	}
	h.mu.Lock()
	prev := h.subs[userID]
	h.subs[userID] = sub
	h.mu.Unlock()
	if prev != nil {
		prev.Close()
	}
	return sub
}

// Unregister removes the subscriber if it is still the active one for its
// user (a replacement registered by a newer stream is left untouched).
func (h *Hub) Unregister(sub *Subscriber) {
	h.mu.Lock()
	if h.subs[sub.UserID] == sub {
		delete(h.subs, sub.UserID)
	}
	h.mu.Unlock()
	sub.Close()
}

// Kick forcibly disconnects a user (admin disable/delete).
func (h *Hub) Kick(userID string) {
	h.mu.RLock()
	sub := h.subs[userID]
	h.mu.RUnlock()
	if sub != nil {
		sub.Close()
	}
}

// SendToUser delivers an event to one user. Returns false if the user is
// offline. A subscriber with a full buffer is closed (client reconnects
// and resyncs) rather than blocking the sender.
func (h *Hub) SendToUser(userID string, ev *quorumv1.ServerEvent) bool {
	h.mu.RLock()
	sub := h.subs[userID]
	h.mu.RUnlock()
	if sub == nil {
		return false
	}
	return sub.send(ev)
}

func (s *Subscriber) send(ev *quorumv1.ServerEvent) bool {
	select {
	case s.Ch <- ev:
		return true
	case <-s.Done:
		return false
	default:
		// Buffer full: terminate the laggard; it will reconnect and resync.
		s.Close()
		return false
	}
}

// FanOut delivers an event to each listed user that is online.
func (h *Hub) FanOut(userIDs []string, ev *quorumv1.ServerEvent) {
	h.mu.RLock()
	targets := make([]*Subscriber, 0, len(userIDs))
	for _, id := range userIDs {
		if sub := h.subs[id]; sub != nil {
			targets = append(targets, sub)
		}
	}
	h.mu.RUnlock()
	for _, sub := range targets {
		sub.send(ev)
	}
}

// Broadcast delivers an event to every online user.
func (h *Hub) Broadcast(ev *quorumv1.ServerEvent) {
	h.mu.RLock()
	targets := make([]*Subscriber, 0, len(h.subs))
	for _, sub := range h.subs {
		targets = append(targets, sub)
	}
	h.mu.RUnlock()
	for _, sub := range targets {
		sub.send(ev)
	}
}

// Online reports whether the user has an active subscription.
func (h *Hub) Online(userID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.subs[userID]
	return ok
}

// OnlineIDs returns the set of online user IDs.
func (h *Hub) OnlineIDs() map[string]bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := make(map[string]bool, len(h.subs))
	for id := range h.subs {
		ids[id] = true
	}
	return ids
}
