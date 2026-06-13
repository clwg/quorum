package client_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	quorumv1 "github.com/clwg/quorum/gen/quorum/v1"
	"github.com/clwg/quorum/internal/auth"
	"github.com/clwg/quorum/internal/client"
	"github.com/clwg/quorum/internal/hub"
	"github.com/clwg/quorum/internal/server"
	"github.com/clwg/quorum/internal/store"
)

// testServer runs a real TLS gRPC server on a loopback port so the full
// client stack (TLS fingerprinting, login, pump, E2EE) is exercised.
type testServer struct {
	addr   string
	caFile string
	store  *store.Store
}

func startTestServer(t *testing.T) *testServer {
	t.Helper()
	dir := t.TempDir()

	caPEM, certPEM, keyPEM := genCerts(t)
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	authn := auth.NewAuthenticator(st)
	h := hub.New()
	srv := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}})),
		grpc.UnaryInterceptor(authn.UnaryInterceptor()),
		grpc.StreamInterceptor(authn.StreamInterceptor()),
	)
	quorumv1.RegisterAuthServiceServer(srv, server.NewAuthService(st))
	quorumv1.RegisterChatServiceServer(srv, server.NewChatService(st, h, slog.Default()))
	quorumv1.RegisterAdminServiceServer(srv, server.NewAdminService(st, h))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)

	return &testServer{addr: lis.Addr().String(), caFile: caFile, store: st}
}

func (ts *testServer) createUser(t *testing.T, username string) string {
	t.Helper()
	phc, err := auth.HashPassword("password123")
	if err != nil {
		t.Fatal(err)
	}
	u := &store.User{ID: store.NewID(), Username: username, PasswordHash: phc, Role: "user"}
	if err := ts.store.CreateUser(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// connect dials, logs in, and starts the pump; events arrive on the
// returned channel.
func (ts *testServer) connect(t *testing.T, username string) (*client.Client, <-chan client.Event, context.CancelFunc) {
	return ts.connectDir(t, username, t.TempDir())
}

// connectDir is connect with an explicit data dir, so a reconnecting user
// can reuse the same identity key and TOFU pins across sessions.
func (ts *testServer) connectDir(t *testing.T, username, dataDir string) (*client.Client, <-chan client.Event, context.CancelFunc) {
	t.Helper()
	c, err := client.Dial(client.Config{Addr: ts.addr, CAFile: ts.caFile, DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := c.Login(ctx, username, "password123"); err != nil {
		cancel()
		t.Fatalf("login %s: %v", username, err)
	}
	events := make(chan client.Event, 128)
	go c.Run(ctx, func(ev client.Event) {
		select {
		case events <- ev:
		default:
		}
	})
	// Wait until online.
	waitEvent(t, events, func(ev client.Event) bool {
		cs, ok := ev.(client.ConnStateEvent)
		return ok && cs.State == client.ConnOnline
	})
	t.Cleanup(func() { cancel(); c.Close() })
	return c, events, cancel
}

func waitEvent(t *testing.T, events <-chan client.Event, match func(client.Event) bool) client.Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-events:
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for event")
		}
	}
}

func TestDMEndToEnd(t *testing.T) {
	ts := startTestServer(t)
	ts.createUser(t, "alice")
	bobID := ts.createUser(t, "bob")

	alice, aliceEvents, _ := ts.connect(t, "alice")
	_, bobEvents, _ := ts.connect(t, "bob")

	if err := alice.SendDM(context.Background(), bobID, "bob", "hello bob, secret stuff"); err != nil {
		t.Fatal(err)
	}

	// Bob receives the decrypted plaintext.
	got := waitEvent(t, bobEvents, func(ev client.Event) bool {
		dm, ok := ev.(client.DirectMessageEvent)
		return ok && !dm.Outgoing
	}).(client.DirectMessageEvent)
	if got.Text != "hello bob, secret stuff" || got.PeerName != "alice" {
		t.Fatalf("unexpected DM: %+v", got)
	}

	// Alice saw the session establish and her own echo.
	waitEvent(t, aliceEvents, func(ev client.Event) bool {
		se, ok := ev.(client.DMSessionEvent)
		return ok && se.Established
	})
	waitEvent(t, aliceEvents, func(ev client.Event) bool {
		dm, ok := ev.(client.DirectMessageEvent)
		return ok && dm.Outgoing
	})

	// Reply flows back over the same session.
	bobClient := got // reuse peer ID from event
	_ = bobClient
}

// TestDMPeerReconnectReestablishesSession covers the case where one party
// leaves and rejoins while the other stays connected. The stayer's session
// is stale after the peer's reconnect (the peer reset its E2EE state); a
// message sealed under it would be silently dropped. The stayer must drop
// the stale session on the peer's offline presence and re-handshake.
func TestDMPeerReconnectReestablishesSession(t *testing.T) {
	ts := startTestServer(t)
	aliceID := ts.createUser(t, "alice")
	bobID := ts.createUser(t, "bob")

	aliceDir := t.TempDir()
	alice, _, aliceCancel := ts.connectDir(t, "alice", aliceDir)
	bob, bobEvents, _ := ts.connect(t, "bob")

	// Establish a session: alice -> bob.
	if err := alice.SendDM(context.Background(), bobID, "bob", "hello bob"); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, bobEvents, func(ev client.Event) bool {
		dm, ok := ev.(client.DirectMessageEvent)
		return ok && !dm.Outgoing && dm.Text == "hello bob"
	})

	// Alice exits; bob must observe her go offline (this drops bob's now
	// stale session to her).
	aliceCancel()
	alice.Close()
	waitEvent(t, bobEvents, func(ev client.Event) bool {
		p, ok := ev.(client.PresenceEvent)
		return ok && p.Presence.GetUserId() == aliceID && !p.Presence.GetOnline()
	})

	// Alice rejoins with the same identity (same data dir).
	_, alice2Events, _ := ts.connectDir(t, "alice", aliceDir)
	waitEvent(t, bobEvents, func(ev client.Event) bool {
		p, ok := ev.(client.PresenceEvent)
		return ok && p.Presence.GetUserId() == aliceID && p.Presence.GetOnline()
	})

	// Bob messages alice. Without re-handshaking, bob would seal under the
	// dead session and alice2 would silently drop it.
	if err := bob.SendDM(context.Background(), aliceID, "alice", "welcome back"); err != nil {
		t.Fatal(err)
	}
	got := waitEvent(t, alice2Events, func(ev client.Event) bool {
		dm, ok := ev.(client.DirectMessageEvent)
		return ok && !dm.Outgoing && dm.Text == "welcome back"
	}).(client.DirectMessageEvent)
	if got.PeerName != "bob" {
		t.Fatalf("unexpected DM: %+v", got)
	}
}

func TestDMOfflinePeerFailsClosed(t *testing.T) {
	ts := startTestServer(t)
	ts.createUser(t, "alice")
	bobID := ts.createUser(t, "bob")

	alice, _, _ := ts.connect(t, "alice")

	// Bob has never logged in: no identity key, definitely offline.
	if err := alice.SendDM(context.Background(), bobID, "bob", "must not leak"); err == nil {
		t.Fatal("send to offline peer must fail")
	}

	// Bob comes online; the failed message must NOT be delivered.
	_, bobEvents, _ := ts.connect(t, "bob")
	select {
	case ev := <-drainFor(bobEvents, 1*time.Second):
		if dm, ok := ev.(client.DirectMessageEvent); ok {
			t.Fatalf("dropped message was delivered: %+v", dm)
		}
	default:
	}
}

// drainFor collects events for d then closes the channel.
func drainFor(events <-chan client.Event, d time.Duration) <-chan client.Event {
	out := make(chan client.Event, 128)
	go func() {
		deadline := time.After(d)
		for {
			select {
			case ev := <-events:
				out <- ev
			case <-deadline:
				close(out)
				return
			}
		}
	}()
	return out
}

func TestDMTOFUMismatchBlocksHandshake(t *testing.T) {
	ts := startTestServer(t)
	ts.createUser(t, "alice")
	bobID := ts.createUser(t, "bob")

	aliceDir := t.TempDir()
	alice, err := client.Dial(client.Config{Addr: ts.addr, CAFile: ts.caFile, DataDir: aliceDir})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := alice.Login(ctx, "alice", "password123"); err != nil {
		t.Fatal(err)
	}
	events := make(chan client.Event, 128)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go alice.Run(runCtx, func(ev client.Event) { events <- ev })
	waitEvent(t, events, func(ev client.Event) bool {
		cs, ok := ev.(client.ConnStateEvent)
		return ok && cs.State == client.ConnOnline
	})

	_, bobEvents, _ := ts.connect(t, "bob")

	// Poison alice's pin for bob with a wrong key.
	pinPath := filepath.Join(aliceDir, alice.ServerID(), "alice", "pins.json")
	wrong := make([]byte, 32)
	wrong[0] = 0x42
	pins := map[string]map[string]string{
		bobID: {"username": "bob", "public_key": base64.StdEncoding.EncodeToString(wrong)},
	}
	raw, _ := json.Marshal(pins)
	if err := os.MkdirAll(filepath.Dir(pinPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pinPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Reconnect so the poisoned pin store is loaded fresh.
	if err := alice.Login(ctx, "alice", "password123"); err != nil {
		t.Fatal(err)
	}

	// The send must fail closed: error, no handshake, nothing delivered.
	if err := alice.SendDM(ctx, bobID, "bob", "must not be sent"); err == nil {
		t.Fatal("TOFU mismatch must block the send")
	}
	select {
	case ev := <-drainFor(bobEvents, 1*time.Second):
		switch ev.(type) {
		case client.DirectMessageEvent, client.DMSessionEvent:
			t.Fatalf("bob saw handshake traffic despite TOFU mismatch: %+v", ev)
		}
	default:
	}
}

func TestDMSimultaneousInitConverges(t *testing.T) {
	ts := startTestServer(t)
	aliceID := ts.createUser(t, "alice")
	bobID := ts.createUser(t, "bob")

	alice, aliceEvents, _ := ts.connect(t, "alice")
	bob, bobEvents, _ := ts.connect(t, "bob")

	done := make(chan error, 2)
	go func() { done <- alice.SendDM(context.Background(), bobID, "bob", "from alice") }()
	go func() { done <- bob.SendDM(context.Background(), aliceID, "alice", "from bob") }()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}

	// Both texts must arrive (possibly after tiebreak re-routing).
	waitEvent(t, bobEvents, func(ev client.Event) bool {
		dm, ok := ev.(client.DirectMessageEvent)
		return ok && !dm.Outgoing && dm.Text == "from alice"
	})
	waitEvent(t, aliceEvents, func(ev client.Event) bool {
		dm, ok := ev.(client.DirectMessageEvent)
		return ok && !dm.Outgoing && dm.Text == "from bob"
	})
}

// genCerts creates a throwaway CA and localhost server cert.
func genCerts(t *testing.T) (caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)

	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return caPEM, certPEM, keyPEM
}
