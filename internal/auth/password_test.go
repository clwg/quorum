package auth

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	phc, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=65536,t=1,p=4$") {
		t.Fatalf("unexpected PHC format: %s", phc)
	}
	if err := VerifyPassword("correct horse battery staple", phc); err != nil {
		t.Fatalf("verify failed: %v", err)
	}
	if err := VerifyPassword("wrong password", phc); err != ErrPasswordMismatch {
		t.Fatalf("want ErrPasswordMismatch, got %v", err)
	}
}

func TestVerifyUsesStoredParams(t *testing.T) {
	// A hash produced with different params must still verify: params are
	// parsed from the PHC string, not assumed from current constants.
	salt := []byte("0123456789abcdef")
	key := argon2.IDKey([]byte("pw12345678"), salt, 2, 32*1024, 2, 32)
	phc := fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, 32*1024, 2, 2,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
	if err := VerifyPassword("pw12345678", phc); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPassword("wrong", phc); err != ErrPasswordMismatch {
		t.Fatalf("want mismatch, got %v", err)
	}
}

func TestVerifyMalformed(t *testing.T) {
	for _, bad := range []string{"", "$argon2id$", "plainhash", "$argon2i$v=19$m=1,t=1,p=1$xx$yy"} {
		if err := VerifyPassword("x", bad); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestTokens(t *testing.T) {
	tok := NewToken(SessionTokenPrefix)
	if !strings.HasPrefix(tok, "qsess_") || len(tok) < 40 {
		t.Fatalf("bad token: %s", tok)
	}
	if IsBotToken(tok) {
		t.Fatal("session token misidentified as bot token")
	}
	if !IsBotToken(NewToken(BotTokenPrefix)) {
		t.Fatal("bot token not identified")
	}
	tok2 := NewToken(SessionTokenPrefix)
	if tok == tok2 {
		t.Fatal("tokens must be unique")
	}
	if len(HashToken(tok)) != 32 {
		t.Fatal("hash must be 32 bytes")
	}
}
