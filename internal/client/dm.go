package client

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"sync"

	quorumv1 "github.com/clwg/quorum/gen/quorum/v1"
	"github.com/clwg/quorum/internal/e2ee"
)

// dmManager owns E2EE state: established sessions, in-flight handshakes,
// and plaintext queues.
//
// Fail-closed rules (see plan §crypto constraints):
//   - Queued plaintext lives only in memory and is flushed only after the
//     handshake completes. Any failure - handshake error, peer offline,
//     TOFU mismatch - drops the queue; nothing is ever sent another way.
//   - A TOFU mismatch blocks the handshake itself (no session, no send).
//   - Simultaneous SESSION_INITs resolve deterministically: the peer with
//     the lexicographically lower user ID is the rightful initiator; the
//     other side's INIT is discarded. No timing dependence.
type dmManager struct {
	c *Client

	mu       sync.Mutex
	sessions map[string]*e2ee.Session // peer ID -> established session
	pending  map[string]*pendingDM    // peer ID -> in-flight initiator handshake
}

type pendingDM struct {
	hs       *e2ee.Handshake
	peerName string
	queue    []string // plaintext awaiting session establishment
}

func newDMManager(c *Client) *dmManager {
	return &dmManager{c: c, sessions: make(map[string]*e2ee.Session), pending: make(map[string]*pendingDM)}
}

// reset drops all sessions and pending handshakes (with their queues).
// Called when the event stream breaks: frames may have been lost, and a
// session must never continue across a gap.
func (m *dmManager) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for peerID, s := range m.sessions {
		s.Close()
		delete(m.sessions, peerID)
	}
	for peerID := range m.pending {
		delete(m.pending, peerID)
	}
}

func (m *dmManager) info(peerID string) (bool, string) {
	m.mu.Lock()
	_, established := m.sessions[peerID]
	m.mu.Unlock()
	fp := ""
	m.c.mu.Lock()
	if m.c.pins != nil {
		m.c.pins.mu.Lock()
		if pin, ok := m.c.pins.pins[peerID]; ok {
			fp = e2ee.Fingerprint(mustB64(pin.PublicKey))
		}
		m.c.pins.mu.Unlock()
	}
	m.c.mu.Unlock()
	return established, fp
}

// send encrypts text for peer, starting a handshake first if necessary.
func (m *dmManager) send(ctx context.Context, peerID, peerName, text string) error {
	m.mu.Lock()
	if sess, ok := m.sessions[peerID]; ok {
		m.mu.Unlock()
		return m.sendOverSession(ctx, sess, peerID, peerName, text)
	}
	if p, ok := m.pending[peerID]; ok {
		// Handshake already in flight: queue and wait.
		p.queue = append(p.queue, text)
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	// Start a new handshake. Directory lookup + TOFU check happen before
	// any state is created; failure leaves nothing queued.
	ident, pins, hctx, err := m.handshakeContext(ctx, peerID, peerName, true)
	if err != nil {
		return err
	}
	keyResp, err := m.c.authc.GetIdentityKey(ctx, &quorumv1.GetIdentityKeyRequest{UserId: peerID})
	if err != nil {
		return fmt.Errorf("identity lookup: %w", err)
	}
	peerKey := keyResp.GetX25519PublicKey()
	if len(peerKey) == 0 {
		return fmt.Errorf("%s has not published an identity key (they must log in at least once)", peerName)
	}
	if err := pins.Check(peerID, peerName, peerKey); err != nil {
		m.c.emit(DMSessionEvent{PeerID: peerID, PeerName: peerName, Err: err})
		return err
	}

	hs, epk, err := e2ee.NewInitiator(ident, peerKey, hctx)
	if err != nil {
		return err
	}

	m.mu.Lock()
	// Re-check: an incoming INIT may have established a session while we
	// were doing the directory lookup.
	if sess, ok := m.sessions[peerID]; ok {
		m.mu.Unlock()
		return m.sendOverSession(ctx, sess, peerID, peerName, text)
	}
	m.pending[peerID] = &pendingDM{hs: hs, peerName: peerName, queue: []string{text}}
	m.mu.Unlock()

	_, err = m.c.chatc.SendDirect(ctx, &quorumv1.SendDirectRequest{Envelope: &quorumv1.DirectEnvelope{
		Type:        quorumv1.DirectEnvelope_TYPE_SESSION_INIT,
		RecipientId: peerID,
		Payload:     epk,
	}})
	if err != nil {
		// Fail closed: drop the pending handshake AND the queued text.
		m.dropPending(peerID)
		return fmt.Errorf("could not reach %s: %w", peerName, err)
	}
	return nil
}

func (m *dmManager) sendOverSession(ctx context.Context, sess *e2ee.Session, peerID, peerName, text string) error {
	ct, counter, err := sess.Seal([]byte(text))
	if err != nil {
		return err
	}
	_, err = m.c.chatc.SendDirect(ctx, &quorumv1.SendDirectRequest{Envelope: &quorumv1.DirectEnvelope{
		Type:        quorumv1.DirectEnvelope_TYPE_MESSAGE,
		RecipientId: peerID,
		SessionId:   sess.ID(),
		Payload:     ct,
		Counter:     counter,
	}})
	if err != nil {
		// The peer likely went offline; the session is no longer usable
		// from a UX standpoint (our counter advanced but delivery failed).
		m.dropSession(peerID, false)
		m.c.emit(DMSessionEvent{PeerID: peerID, PeerName: peerName, Err: fmt.Errorf("delivery failed: %w", err)})
		return err
	}
	m.c.emit(DirectMessageEvent{PeerID: peerID, PeerName: peerName, Text: text, Outgoing: true})
	return nil
}

// handshakeContext snapshots identity/pins and builds the e2ee.Context
// with canonical initiator/responder roles.
func (m *dmManager) handshakeContext(_ context.Context, peerID, peerName string, weInitiate bool) (ident *ecdh.PrivateKey, pins *pinStore, hctx e2ee.Context, err error) {
	m.c.mu.Lock()
	ident = m.c.identity
	pins = m.c.pins
	ourID, ourName := m.c.userID, m.c.username
	m.c.mu.Unlock()
	if ident == nil || pins == nil {
		return nil, nil, e2ee.Context{}, errors.New("not logged in")
	}
	hctx = e2ee.Context{ServerID: m.c.serverID}
	if weInitiate {
		hctx.InitiatorID, hctx.InitiatorUsername = ourID, ourName
		hctx.ResponderID, hctx.ResponderUsername = peerID, peerName
	} else {
		hctx.InitiatorID, hctx.InitiatorUsername = peerID, peerName
		hctx.ResponderID, hctx.ResponderUsername = ourID, ourName
	}
	return ident, pins, hctx, nil
}

func (m *dmManager) dropPending(peerID string) {
	m.mu.Lock()
	delete(m.pending, peerID)
	m.mu.Unlock()
}

// peerOffline drops our session and any in-flight handshake with a peer
// that has gone offline. When the peer reconnects it resets its own E2EE
// state (see reset), so our session is already dead - the same fail-closed
// rule reset() applies to gaps in our own stream, applied to the peer's.
// The next send starts a fresh handshake; nothing stale is ever sealed.
func (m *dmManager) peerOffline(peerID, peerName string) {
	m.mu.Lock()
	sess := m.sessions[peerID]
	delete(m.sessions, peerID)
	delete(m.pending, peerID)
	m.mu.Unlock()
	if sess != nil {
		sess.Close()
		m.c.emit(DMSessionEvent{PeerID: peerID, PeerName: peerName, Established: false})
	}
}

func (m *dmManager) dropSession(peerID string, notifyPeer bool) {
	m.mu.Lock()
	sess := m.sessions[peerID]
	delete(m.sessions, peerID)
	m.mu.Unlock()
	if sess != nil {
		if notifyPeer {
			_, _ = m.c.chatc.SendDirect(context.Background(), &quorumv1.SendDirectRequest{Envelope: &quorumv1.DirectEnvelope{
				Type:        quorumv1.DirectEnvelope_TYPE_SESSION_CLOSE,
				RecipientId: peerID,
				SessionId:   sess.ID(),
			}})
		}
		sess.Close()
	}
}

// handleEnvelope processes an inbound DirectEnvelope from the stream pump.
func (m *dmManager) handleEnvelope(ctx context.Context, env *quorumv1.DirectEnvelope) {
	peerID, peerName := env.GetSenderId(), env.GetSenderName()
	switch env.GetType() {
	case quorumv1.DirectEnvelope_TYPE_SESSION_INIT:
		m.handleInit(ctx, env, peerID, peerName)
	case quorumv1.DirectEnvelope_TYPE_SESSION_ACCEPT:
		m.handleAccept(env, peerID, peerName)
	case quorumv1.DirectEnvelope_TYPE_MESSAGE:
		m.handleMessage(env, peerID, peerName)
	case quorumv1.DirectEnvelope_TYPE_SESSION_CLOSE:
		m.mu.Lock()
		if sess := m.sessions[peerID]; sess != nil {
			sess.Close()
			delete(m.sessions, peerID)
		}
		m.mu.Unlock()
		m.c.emit(DMSessionEvent{PeerID: peerID, PeerName: peerName, Established: false})
	}
}

func (m *dmManager) handleInit(ctx context.Context, env *quorumv1.DirectEnvelope, peerID, peerName string) {
	ourID := m.c.UserID()

	// Simultaneous-init tiebreak: lower user ID is the rightful initiator.
	m.mu.Lock()
	if pend, exists := m.pending[peerID]; exists {
		if ourID < peerID {
			// We are rightful: discard their INIT; our INIT will win on
			// their side under the same rule.
			m.mu.Unlock()
			return
		}
		// They are rightful: abandon our handshake but keep the queued
		// text - it will flush once their session establishes.
		queued := pend.queue
		delete(m.pending, peerID)
		m.mu.Unlock()
		m.acceptInit(ctx, env, peerID, peerName, queued)
		return
	}
	m.mu.Unlock()
	m.acceptInit(ctx, env, peerID, peerName, nil)
}

func (m *dmManager) acceptInit(ctx context.Context, env *quorumv1.DirectEnvelope, peerID, peerName string, carryQueue []string) {
	ident, pins, hctx, err := m.handshakeContext(ctx, peerID, peerName, false)
	if err != nil {
		return
	}
	keyResp, err := m.c.authc.GetIdentityKey(ctx, &quorumv1.GetIdentityKeyRequest{UserId: peerID})
	if err != nil || len(keyResp.GetX25519PublicKey()) == 0 {
		m.c.emit(DMSessionEvent{PeerID: peerID, PeerName: peerName, Err: fmt.Errorf("identity lookup failed: %v", err)})
		return
	}
	peerKey := keyResp.GetX25519PublicKey()
	// TOFU mismatch blocks the handshake; we never accept the session.
	if err := pins.Check(peerID, peerName, peerKey); err != nil {
		m.c.emit(DMSessionEvent{PeerID: peerID, PeerName: peerName, Err: err})
		return
	}

	sess, epk, err := e2ee.Accept(ident, peerKey, env.GetPayload(), hctx)
	if err != nil {
		m.c.emit(DMSessionEvent{PeerID: peerID, PeerName: peerName, Err: err})
		return
	}
	if _, err := m.c.chatc.SendDirect(ctx, &quorumv1.SendDirectRequest{Envelope: &quorumv1.DirectEnvelope{
		Type:        quorumv1.DirectEnvelope_TYPE_SESSION_ACCEPT,
		RecipientId: peerID,
		SessionId:   sess.ID(),
		Payload:     epk,
	}}); err != nil {
		sess.Close()
		m.c.emit(DMSessionEvent{PeerID: peerID, PeerName: peerName, Err: err})
		return
	}

	m.installSession(ctx, peerID, peerName, sess, peerKey, carryQueue)
}

func (m *dmManager) handleAccept(env *quorumv1.DirectEnvelope, peerID, peerName string) {
	m.mu.Lock()
	pend, ok := m.pending[peerID]
	if !ok {
		m.mu.Unlock()
		return // unsolicited or superseded ACCEPT
	}
	delete(m.pending, peerID)
	m.mu.Unlock()

	sess, err := pend.hs.Complete(env.GetPayload())
	if err != nil {
		// Fail closed: the queue dies with the handshake.
		m.c.emit(DMSessionEvent{PeerID: peerID, PeerName: peerName, Err: err})
		return
	}
	m.installSession(context.Background(), peerID, pend.peerName, sess, nil, pend.queue)
}

// installSession records an established session, emits the lifecycle
// event, and flushes any queued plaintext through it.
func (m *dmManager) installSession(ctx context.Context, peerID, peerName string, sess *e2ee.Session, peerKey []byte, queue []string) {
	m.mu.Lock()
	if old := m.sessions[peerID]; old != nil {
		old.Close()
	}
	m.sessions[peerID] = sess
	m.mu.Unlock()

	fp := ""
	if peerKey != nil {
		fp = e2ee.Fingerprint(peerKey)
	} else {
		_, fp = m.info(peerID)
	}
	m.c.emit(DMSessionEvent{PeerID: peerID, PeerName: peerName, Established: true, Fingerprint: fp})

	for _, text := range queue {
		if err := m.sendOverSession(ctx, sess, peerID, peerName, text); err != nil {
			m.c.emit(ErrorEvent{Context: "sending queued message to " + peerName, Err: err})
			return
		}
	}
}

func (m *dmManager) handleMessage(env *quorumv1.DirectEnvelope, peerID, peerName string) {
	m.mu.Lock()
	sess := m.sessions[peerID]
	m.mu.Unlock()
	if sess == nil {
		m.c.emit(ErrorEvent{Context: "direct message from " + peerName, Err: errors.New("no session (message dropped); ask them to resend")})
		return
	}
	pt, err := sess.Open(env.GetPayload(), env.GetCounter())
	if err != nil {
		// Decrypt failure or replay: the session is unsafe to continue.
		m.dropSession(peerID, true)
		m.c.emit(DMSessionEvent{PeerID: peerID, PeerName: peerName, Err: fmt.Errorf("decryption failed, session closed: %w", err)})
		return
	}
	m.c.emit(DirectMessageEvent{PeerID: peerID, PeerName: peerName, Text: string(pt)})
}
