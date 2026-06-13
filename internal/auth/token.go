package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

const (
	SessionTokenPrefix = "qsess_"
	BotTokenPrefix     = "qbot_"
)

// NewToken mints a random bearer token with the given prefix.
func NewToken(prefix string) string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:])
}

// HashToken returns sha256(token); only hashes are stored server-side.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func IsBotToken(token string) bool {
	return strings.HasPrefix(token, BotTokenPrefix)
}
