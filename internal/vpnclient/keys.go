package vpnclient

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/curve25519"
)

const KeyLen = 32

func GenerateKey() (KeyPair, error) {
	var priv [KeyLen]byte
	if _, err := rand.Read(priv[:]); err != nil {
		return KeyPair{}, fmt.Errorf("read random bytes for a WireGuard key: %w", err)
	}
	clamp(&priv)
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return KeyPair{}, fmt.Errorf("derive the public key: %w", err)
	}
	return KeyPair{
		Private: base64.StdEncoding.EncodeToString(priv[:]),
		Public:  base64.StdEncoding.EncodeToString(pub),
	}, nil
}

func PublicKey(private string) (string, error) {
	priv, err := ParseKey(private)
	if err != nil {
		return "", err
	}
	clamp(&priv)
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return "", fmt.Errorf("derive the public key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(pub), nil
}

func ParseKey(s string) ([KeyLen]byte, error) {
	var k [KeyLen]byte
	s = strings.TrimSpace(s)
	if s == "" {
		return k, fmt.Errorf("empty WireGuard key")
	}
	if len(s) == 2*KeyLen && isHex(s) {
		b, err := hex.DecodeString(s)
		if err != nil {
			return k, fmt.Errorf("parse WireGuard key: %w", err)
		}
		copy(k[:], b)
		return k, nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return k, fmt.Errorf("parse WireGuard key %q: not 64 hex characters and not base64: %w", elide(s), err)
	}
	if len(b) != KeyLen {
		return k, fmt.Errorf("parse WireGuard key: got %d bytes, want %d", len(b), KeyLen)
	}
	copy(k[:], b)
	return k, nil
}

func KeyHex(s string) (string, error) {
	k, err := ParseKey(s)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(k[:]), nil
}

func KeyBase64(s string) (string, error) {
	k, err := ParseKey(s)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k[:]), nil
}

func ValidKey(s string) bool {
	_, err := ParseKey(s)
	return err == nil
}

func clamp(k *[KeyLen]byte) {
	k[0] &= 248
	k[KeyLen-1] = (k[KeyLen-1] & 127) | 64
}

func isHex(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func elide(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8] + "…"
}
