// Package e2ee implements quorum's end-to-end encryption for 1:1 chats:
// a synchronous triple-DH handshake over X25519 followed by per-direction
// ChaCha20-Poly1305 framing with counter nonces.
//
// Security properties and limitations:
//   - dh1 (ephemeral-ephemeral) gives forward secrecy: compromise of a
//     long-term identity key exposes no past traffic.
//   - dh2/dh3 bind both long-term identity keys, authenticating each side
//     to the other, assuming identity keys are verified out-of-band or
//     TOFU-pinned (the server directory is trusted on first use only).
//   - There is no ratchet: compromise of a session key exposes that
//     session only. Fresh ephemerals are REQUIRED per session; a Session
//     can only be obtained from a completed handshake, never constructed
//     or rekeyed, so counter nonces can never repeat under a key.
//   - The transcript binds the server identity and both usernames, so a
//     handshake in one server context cannot be confused with another.
//
// The package is pure: no I/O, no gRPC types. Entropy comes only from
// crypto/rand; there is deliberately no injectable randomness.
package e2ee

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"

	"crypto/hkdf"
)

const protocolLabel = "quorum-e2ee-v1"

// Context binds a handshake to its server and participants. Initiator is
// always "A": the sender of SESSION_INIT.
type Context struct {
	ServerID          string // fingerprint of the server's TLS public key
	InitiatorID       string // user ID of the SESSION_INIT sender
	InitiatorUsername string
	ResponderID       string
	ResponderUsername string
}

var (
	ErrHandshakeFailed = errors.New("e2ee: handshake failed")
	ErrReplay          = errors.New("e2ee: counter replay or regression")
	ErrDecrypt         = errors.New("e2ee: decryption failed")
)

// GenerateIdentityKey creates a long-term X25519 identity keypair.
func GenerateIdentityKey() (*ecdh.PrivateKey, error) {
	return ecdh.X25519().GenerateKey(rand.Reader)
}

// ParsePrivateKey loads a 32-byte X25519 private key.
func ParsePrivateKey(b []byte) (*ecdh.PrivateKey, error) {
	return ecdh.X25519().NewPrivateKey(b)
}

// Handshake is the initiator's in-flight state between sending
// SESSION_INIT and receiving SESSION_ACCEPT.
type Handshake struct {
	identity  *ecdh.PrivateKey
	peerIK    *ecdh.PublicKey
	ephemeral *ecdh.PrivateKey
	ctx       Context
}

// NewInitiator starts a handshake as A. It returns the in-flight state and
// the ephemeral public key to send in SESSION_INIT.
func NewInitiator(identity *ecdh.PrivateKey, peerIdentityPub []byte, hctx Context) (*Handshake, []byte, error) {
	peerIK, err := ecdh.X25519().NewPublicKey(peerIdentityPub)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: bad peer identity key: %v", ErrHandshakeFailed, err)
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	return &Handshake{identity: identity, peerIK: peerIK, ephemeral: eph, ctx: hctx},
		eph.PublicKey().Bytes(), nil
}

// Complete finishes the initiator handshake with the responder's ephemeral
// public key from SESSION_ACCEPT.
func (h *Handshake) Complete(responderEphemeralPub []byte) (*Session, error) {
	epkB, err := ecdh.X25519().NewPublicKey(responderEphemeralPub)
	if err != nil {
		return nil, fmt.Errorf("%w: bad responder ephemeral: %v", ErrHandshakeFailed, err)
	}
	keys, err := deriveKeys(h.ctx,
		h.identity, h.ephemeral, // our private keys (A)
		h.peerIK, epkB, // peer public keys (B)
		true)
	if err != nil {
		return nil, err
	}
	return newSession(keys, h.ctx, true)
}

// Accept performs the responder (B) side in one step: given the
// initiator's ephemeral from SESSION_INIT, it returns the established
// session and the ephemeral public key to send back in SESSION_ACCEPT.
func Accept(identity *ecdh.PrivateKey, peerIdentityPub, initiatorEphemeralPub []byte, hctx Context) (*Session, []byte, error) {
	peerIK, err := ecdh.X25519().NewPublicKey(peerIdentityPub)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: bad peer identity key: %v", ErrHandshakeFailed, err)
	}
	epkA, err := ecdh.X25519().NewPublicKey(initiatorEphemeralPub)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: bad initiator ephemeral: %v", ErrHandshakeFailed, err)
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	keys, err := deriveKeys(hctx,
		identity, eph, // our private keys (B)
		peerIK, epkA, // peer public keys (A)
		false)
	if err != nil {
		return nil, nil, err
	}
	sess, err := newSession(keys, hctx, false)
	if err != nil {
		return nil, nil, err
	}
	return sess, eph.PublicKey().Bytes(), nil
}

type derivedKeys struct {
	sessionID [16]byte
	a2b, b2a  []byte
}

// deriveKeys runs the three DH computations and the KDF. All three DH
// calls happen here and any error aborts the whole derivation: Go's
// crypto/ecdh rejects low-order points (all-zero shared secrets), and no
// partial result can escape this function.
func deriveKeys(hctx Context, ourIK, ourEph *ecdh.PrivateKey, peerIK, peerEph *ecdh.PublicKey, weAreInitiator bool) (*derivedKeys, error) {
	// dh1: ephemeral-ephemeral (forward secrecy)
	dh1, err := ourEph.ECDH(peerEph)
	if err != nil {
		return nil, fmt.Errorf("%w: dh1: %v", ErrHandshakeFailed, err)
	}
	// dh2: X25519(ikA, epkB); dh3: X25519(epkA, ikB) - fixed A/B roles.
	var dh2, dh3 []byte
	if weAreInitiator {
		dh2, err = ourIK.ECDH(peerEph)
		if err != nil {
			return nil, fmt.Errorf("%w: dh2: %v", ErrHandshakeFailed, err)
		}
		dh3, err = ourEph.ECDH(peerIK)
		if err != nil {
			return nil, fmt.Errorf("%w: dh3: %v", ErrHandshakeFailed, err)
		}
	} else {
		dh2, err = ourEph.ECDH(peerIK)
		if err != nil {
			return nil, fmt.Errorf("%w: dh2: %v", ErrHandshakeFailed, err)
		}
		dh3, err = ourIK.ECDH(peerEph)
		if err != nil {
			return nil, fmt.Errorf("%w: dh3: %v", ErrHandshakeFailed, err)
		}
	}

	// Transcript: canonical A-then-B ordering, length-prefixed fields.
	var ikA, ikB, epkA, epkB []byte
	if weAreInitiator {
		ikA, epkA = ourIK.PublicKey().Bytes(), ourEph.PublicKey().Bytes()
		ikB, epkB = peerIK.Bytes(), peerEph.Bytes()
	} else {
		ikA, epkA = peerIK.Bytes(), peerEph.Bytes()
		ikB, epkB = ourIK.PublicKey().Bytes(), ourEph.PublicKey().Bytes()
	}
	transcript := sha256.New()
	for _, field := range [][]byte{
		[]byte(protocolLabel),
		[]byte(hctx.ServerID),
		[]byte(hctx.InitiatorID), []byte(hctx.InitiatorUsername),
		[]byte(hctx.ResponderID), []byte(hctx.ResponderUsername),
		ikA, ikB, epkA, epkB,
	} {
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(field)))
		transcript.Write(lenBuf[:])
		transcript.Write(field)
	}
	salt := transcript.Sum(nil)

	ikm := make([]byte, 0, len(dh1)+len(dh2)+len(dh3))
	ikm = append(ikm, dh1...)
	ikm = append(ikm, dh2...)
	ikm = append(ikm, dh3...)

	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	a2b, err := hkdf.Expand(sha256.New, prk, "a2b", chacha20poly1305.KeySize)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	b2a, err := hkdf.Expand(sha256.New, prk, "b2a", chacha20poly1305.KeySize)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	sid, err := hkdf.Expand(sha256.New, prk, "sid", 16)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHandshakeFailed, err)
	}
	keys := &derivedKeys{a2b: a2b, b2a: b2a}
	copy(keys.sessionID[:], sid)
	return keys, nil
}

// Session is an established E2EE channel with one peer. Sessions are only
// created by a completed handshake; there is no other constructor and no
// way to rekey one - open a new session instead.
type Session struct {
	id        [16]byte
	localID   string // our user ID (AAD sender for Seal)
	peerID    string // peer user ID (AAD sender for Open)
	mu        sync.Mutex
	send      *aeadState
	recv      *aeadState
	closed    bool
	Initiator bool
}

type aeadState struct {
	key     []byte
	counter uint64 // send: next to use; recv: lowest acceptable
}

func newSession(keys *derivedKeys, hctx Context, weAreInitiator bool) (*Session, error) {
	s := &Session{id: keys.sessionID, Initiator: weAreInitiator}
	if weAreInitiator {
		s.localID, s.peerID = hctx.InitiatorID, hctx.ResponderID
		s.send = &aeadState{key: keys.a2b}
		s.recv = &aeadState{key: keys.b2a}
	} else {
		s.localID, s.peerID = hctx.ResponderID, hctx.InitiatorID
		s.send = &aeadState{key: keys.b2a}
		s.recv = &aeadState{key: keys.a2b}
	}
	return s, nil
}

// ID returns the 16-byte session identifier.
func (s *Session) ID() []byte { return s.id[:] }

// PeerID returns the peer's user ID.
func (s *Session) PeerID() string { return s.peerID }

func nonceFor(counter uint64) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(nonce[4:], counter)
	return nonce
}

func aad(sessionID [16]byte, senderID string, counter uint64) []byte {
	out := make([]byte, 0, 16+len(senderID)+8)
	out = append(out, sessionID[:]...)
	out = append(out, senderID...)
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], counter)
	return append(out, c[:]...)
}

// Seal encrypts plaintext for the peer, returning the ciphertext and the
// counter that must accompany it on the wire.
func (s *Session) Seal(plaintext []byte) (ciphertext []byte, counter uint64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, 0, errors.New("e2ee: session closed")
	}
	aead, err := chacha20poly1305.New(s.send.key)
	if err != nil {
		return nil, 0, err
	}
	counter = s.send.counter
	s.send.counter++
	ct := aead.Seal(nil, nonceFor(counter), plaintext, aad(s.id, s.localID, counter))
	return ct, counter, nil
}

// Open decrypts a peer ciphertext. Counters must be strictly increasing;
// replays and regressions are rejected before any decryption attempt.
func (s *Session) Open(ciphertext []byte, counter uint64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("e2ee: session closed")
	}
	if counter < s.recv.counter {
		return nil, ErrReplay
	}
	aead, err := chacha20poly1305.New(s.recv.key)
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(nil, nonceFor(counter), ciphertext, aad(s.id, s.peerID, counter))
	if err != nil {
		return nil, ErrDecrypt
	}
	s.recv.counter = counter + 1
	return pt, nil
}

// Close renders the session unusable and zeroizes key material.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for i := range s.send.key {
		s.send.key[i] = 0
	}
	for i := range s.recv.key {
		s.recv.key[i] = 0
	}
}

// Fingerprint renders the first 16 bytes of SHA-256(pubkey) as grouped hex
// for out-of-band identity verification.
func Fingerprint(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	out := make([]byte, 0, 16*2+7)
	const hexdigits = "0123456789abcdef"
	for i, b := range sum[:16] {
		if i > 0 && i%2 == 0 {
			out = append(out, ' ')
		}
		out = append(out, hexdigits[b>>4], hexdigits[b&0xf])
	}
	return string(out)
}
