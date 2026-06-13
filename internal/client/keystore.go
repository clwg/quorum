package client

import (
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/layer8/quorum/internal/e2ee"
)

// userDir returns the per-server, per-user state directory. Scoping by
// server fingerprint keeps identities and pins from leaking across
// servers (usernames are server-local).
func userDir(dataDir, serverID, username string) string {
	return filepath.Join(dataDir, serverID, username)
}

// loadOrCreateIdentity loads the long-term X25519 identity key for this
// (server, user), generating one on first use. A fresh key per server
// also prevents servers from correlating a user across deployments.
func loadOrCreateIdentity(dataDir, serverID, username string) (*ecdh.PrivateKey, error) {
	dir := userDir(dataDir, serverID, username)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "identity.key")
	if raw, err := os.ReadFile(path); err == nil {
		key, err := e2ee.ParsePrivateKey(raw)
		if err != nil {
			return nil, fmt.Errorf("corrupt identity key at %s: %w", path, err)
		}
		return key, nil
	}
	key, err := e2ee.GenerateIdentityKey()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key.Bytes(), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// pinStore holds TOFU pins: the first identity key seen for each peer on
// this server. A later mismatch fails closed until the user removes the
// pin manually after out-of-band verification.
type pinStore struct {
	mu   sync.Mutex
	path string
	pins map[string]pinEntry // peer user ID -> pin
}

type pinEntry struct {
	Username  string `json:"username"`
	PublicKey string `json:"public_key"` // base64
}

func openPinStore(dataDir, serverID, username string) (*pinStore, error) {
	dir := userDir(dataDir, serverID, username)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ps := &pinStore{path: filepath.Join(dir, "pins.json"), pins: make(map[string]pinEntry)}
	raw, err := os.ReadFile(ps.path)
	if os.IsNotExist(err) {
		return ps, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &ps.pins); err != nil {
		return nil, fmt.Errorf("corrupt pin store at %s: %w", ps.path, err)
	}
	return ps, nil
}

// Check verifies a peer's identity key against the pin, pinning it on
// first use. It returns an error on mismatch (possible MITM or key
// rotation - must be resolved out-of-band).
func (ps *pinStore) Check(peerID, peerName string, publicKey []byte) error {
	enc := base64.StdEncoding.EncodeToString(publicKey)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if cur, ok := ps.pins[peerID]; ok {
		if cur.PublicKey != enc {
			return fmt.Errorf("identity key for %s changed (pinned %s, got %s) - verify out-of-band, then remove the entry from %s to re-pin",
				peerName,
				e2ee.Fingerprint(mustB64(cur.PublicKey)),
				e2ee.Fingerprint(publicKey),
				ps.path)
		}
		return nil
	}
	ps.pins[peerID] = pinEntry{Username: peerName, PublicKey: enc}
	return ps.saveLocked()
}

func (ps *pinStore) saveLocked() error {
	raw, err := json.MarshalIndent(ps.pins, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ps.path, raw, 0o600)
}

func mustB64(s string) []byte {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}
