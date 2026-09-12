package fleet

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const TokenTTL = time.Hour

const MaxTokenTTL = 24 * time.Hour

const TokenBytes = 32

var (
	ErrTokenUnknown = errors.New("unknown join token")

	ErrTokenUsed = errors.New("join token already used")

	ErrTokenExpired = errors.New("join token expired")
)

type Token struct {
	Hash string `json:"hash"`

	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`

	UsedBy string    `json:"used_by,omitempty"`
	UsedAt time.Time `json:"used_at,omitempty"`
}

func (t Token) Used() bool { return t.UsedBy != "" || !t.UsedAt.IsZero() }

func (t Token) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt)
}

func (t Token) Check(now time.Time) error {
	switch {
	case t.Hash == "":
		return ErrTokenUnknown
	case t.Used():
		return ErrTokenUsed
	case t.Expired(now):
		return ErrTokenExpired
	}
	return nil
}

func Hash(secret string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(secret)))
	return hex.EncodeToString(sum[:])
}

func NewToken(secret, createdBy string, now time.Time, ttl time.Duration) (Token, error) {
	if strings.TrimSpace(secret) == "" {
		return Token{}, errors.New("a join token needs a secret")
	}
	switch {
	case ttl == 0:
		ttl = TokenTTL
	case ttl < 0:
		return Token{}, errors.New("a join token's lifetime must not be negative")
	case ttl > MaxTokenTTL:
		return Token{}, errors.New("a join token may live at most " + MaxTokenTTL.String())
	}
	return Token{
		Hash:      Hash(secret),
		CreatedBy: createdBy,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}, nil
}

func NewSecret() (string, error) {
	b := make([]byte, TokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("fleet: read random for a join token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
