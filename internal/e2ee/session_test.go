package e2ee

import (
	"bytes"
	"errors"
	"testing"
)

func testContext() Context {
	return Context{
		ServerID:          "srv-fingerprint",
		InitiatorID:       "user-a",
		InitiatorUsername: "alice",
		ResponderID:       "user-b",
		ResponderUsername: "bob",
	}
}

// establish runs a full A<->B handshake and returns both sessions.
func establish(t *testing.T, hctx Context) (*Session, *Session) {
	t.Helper()
	ikA, err := GenerateIdentityKey()
	if err != nil {
		t.Fatal(err)
	}
	ikB, err := GenerateIdentityKey()
	if err != nil {
		t.Fatal(err)
	}
	hs, epkA, err := NewInitiator(ikA, ikB.PublicKey().Bytes(), hctx)
	if err != nil {
		t.Fatal(err)
	}
	sessB, epkB, err := Accept(ikB, ikA.PublicKey().Bytes(), epkA, hctx)
	if err != nil {
		t.Fatal(err)
	}
	sessA, err := hs.Complete(epkB)
	if err != nil {
		t.Fatal(err)
	}
	return sessA, sessB
}

func TestHandshakeAgreementAndRoundTrip(t *testing.T) {
	sessA, sessB := establish(t, testContext())
	if !bytes.Equal(sessA.ID(), sessB.ID()) {
		t.Fatal("session IDs disagree")
	}

	// A -> B
	ct, ctr, err := sessA.Seal([]byte("hello bob"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := sessB.Open(ct, ctr)
	if err != nil {
		t.Fatal(err)
	}
	if string(pt) != "hello bob" {
		t.Fatalf("got %q", pt)
	}

	// B -> A (independent direction)
	ct2, ctr2, err := sessB.Seal([]byte("hi alice"))
	if err != nil {
		t.Fatal(err)
	}
	pt2, err := sessA.Open(ct2, ctr2)
	if err != nil {
		t.Fatal(err)
	}
	if string(pt2) != "hi alice" {
		t.Fatalf("got %q", pt2)
	}

	// Sustained two-way traffic with monotonic counters.
	for i := range 10 {
		ct, ctr, _ := sessA.Seal([]byte("ping"))
		if _, err := sessB.Open(ct, ctr); err != nil {
			t.Fatalf("msg %d: %v", i, err)
		}
	}
}

func TestTamperDetection(t *testing.T) {
	sessA, sessB := establish(t, testContext())
	ct, ctr, _ := sessA.Seal([]byte("secret"))
	ct[len(ct)/2] ^= 0x01
	if _, err := sessB.Open(ct, ctr); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("want ErrDecrypt, got %v", err)
	}
}

func TestReplayAndRegressionRejected(t *testing.T) {
	sessA, sessB := establish(t, testContext())
	ct1, ctr1, _ := sessA.Seal([]byte("one"))
	ct2, ctr2, _ := sessA.Seal([]byte("two"))

	if _, err := sessB.Open(ct1, ctr1); err != nil {
		t.Fatal(err)
	}
	// Exact replay.
	if _, err := sessB.Open(ct1, ctr1); !errors.Is(err, ErrReplay) {
		t.Fatalf("replay: want ErrReplay, got %v", err)
	}
	if _, err := sessB.Open(ct2, ctr2); err != nil {
		t.Fatal(err)
	}
	// Regression to an older counter.
	if _, err := sessB.Open(ct1, ctr1); !errors.Is(err, ErrReplay) {
		t.Fatalf("regression: want ErrReplay, got %v", err)
	}
	// Valid ciphertext relabeled with a future counter must fail AEAD.
	ct3, ctr3, _ := sessA.Seal([]byte("three"))
	if _, err := sessB.Open(ct3, ctr3+5); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("counter mismatch: want ErrDecrypt, got %v", err)
	}
}

func TestWrongIdentityRejected(t *testing.T) {
	// B accepts using mallory's identity in place of alice's; the
	// transcript and dh2/dh3 disagree, so traffic must not decrypt.
	hctx := testContext()
	ikA, _ := GenerateIdentityKey()
	ikB, _ := GenerateIdentityKey()
	ikMallory, _ := GenerateIdentityKey()

	hs, epkA, err := NewInitiator(ikA, ikB.PublicKey().Bytes(), hctx)
	if err != nil {
		t.Fatal(err)
	}
	sessB, epkB, err := Accept(ikB, ikMallory.PublicKey().Bytes(), epkA, hctx)
	if err != nil {
		t.Fatal(err)
	}
	sessA, err := hs.Complete(epkB)
	if err != nil {
		t.Fatal(err)
	}
	ct, ctr, _ := sessA.Seal([]byte("for bob only"))
	if _, err := sessB.Open(ct, ctr); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("want ErrDecrypt with mismatched identities, got %v", err)
	}
}

func TestContextBinding(t *testing.T) {
	// Identical keys but a different server context must derive different
	// sessions (channel binding).
	ikA, _ := GenerateIdentityKey()
	ikB, _ := GenerateIdentityKey()

	ctx1 := testContext()
	ctx2 := testContext()
	ctx2.ServerID = "other-server"

	hs, epkA, err := NewInitiator(ikA, ikB.PublicKey().Bytes(), ctx1)
	if err != nil {
		t.Fatal(err)
	}
	sessB, epkB, err := Accept(ikB, ikA.PublicKey().Bytes(), epkA, ctx2)
	if err != nil {
		t.Fatal(err)
	}
	sessA, err := hs.Complete(epkB)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(sessA.ID(), sessB.ID()) {
		t.Fatal("sessions in different server contexts must not match")
	}
	ct, ctr, _ := sessA.Seal([]byte("x"))
	if _, err := sessB.Open(ct, ctr); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("want ErrDecrypt across contexts, got %v", err)
	}
}

func TestLowOrderPointAborts(t *testing.T) {
	// The all-zero point is low-order: every DH involving it yields the
	// all-zero shared secret, which crypto/ecdh rejects. The handshake
	// must hard-abort.
	hctx := testContext()
	ikA, _ := GenerateIdentityKey()
	ikB, _ := GenerateIdentityKey()
	lowOrder := make([]byte, 32)

	// As responder: low-order initiator ephemeral.
	if _, _, err := Accept(ikB, ikA.PublicKey().Bytes(), lowOrder, hctx); err == nil {
		t.Fatal("Accept must abort on low-order initiator ephemeral")
	}

	// As initiator: low-order responder ephemeral.
	hs, _, err := NewInitiator(ikA, ikB.PublicKey().Bytes(), hctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hs.Complete(lowOrder); err == nil {
		t.Fatal("Complete must abort on low-order responder ephemeral")
	}

	// Low-order identity key served by a malicious directory: the DH
	// involving it (dh3 on the initiator side) must abort in Complete.
	hs2, epkA2, err := NewInitiator(ikA, lowOrder, hctx)
	if err == nil {
		_, epkB, errAccept := Accept(ikB, ikA.PublicKey().Bytes(), epkA2, hctx)
		if errAccept != nil {
			t.Fatal(errAccept)
		}
		if _, errComplete := hs2.Complete(epkB); errComplete == nil {
			t.Fatal("handshake with low-order identity key must abort")
		}
	}
}

func TestSessionKeyUniqueness(t *testing.T) {
	// Tripwire for the fresh-ephemeral invariant: successive sessions
	// between the same peers must never share IDs (and therefore keys).
	hctx := testContext()
	seen := make(map[string]bool)
	for range 8 {
		sessA, sessB := establish(t, hctx)
		id := string(sessA.ID())
		if seen[id] {
			t.Fatal("session ID repeated across handshakes")
		}
		seen[id] = true
		_ = sessB
	}
}

func TestClosedSessionUnusable(t *testing.T) {
	sessA, sessB := establish(t, testContext())
	ct, ctr, _ := sessA.Seal([]byte("x"))
	sessB.Close()
	if _, err := sessB.Open(ct, ctr); err == nil {
		t.Fatal("closed session must not decrypt")
	}
	sessA.Close()
	if _, _, err := sessA.Seal([]byte("y")); err == nil {
		t.Fatal("closed session must not encrypt")
	}
}

func TestFingerprint(t *testing.T) {
	ik, _ := GenerateIdentityKey()
	fp := Fingerprint(ik.PublicKey().Bytes())
	if len(fp) != 39 { // 16 bytes -> 32 hex chars + 7 spaces
		t.Fatalf("unexpected fingerprint format: %q", fp)
	}
}
