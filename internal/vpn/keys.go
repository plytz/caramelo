package vpn

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/curve25519"
)

const KeyLen = 32

type Key [KeyLen]byte

func GenerateKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return Key{}, fmt.Errorf("vpn: read random: %w", err)
	}
	return k.clamped(), nil
}

func (k Key) clamped() Key {
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	return k
}

func (k Key) Public() (Key, error) {
	b, err := curve25519.X25519(k[:], curve25519.Basepoint)
	if err != nil {
		return Key{}, fmt.Errorf("vpn: derive public key: %w", err)
	}
	var pub Key
	copy(pub[:], b)
	return pub, nil
}

func (k Key) Base64() string { return base64.StdEncoding.EncodeToString(k[:]) }

func (k Key) Hex() string { return hex.EncodeToString(k[:]) }

func (k Key) String() string { return k.Base64() }

func (k Key) IsZero() bool { return k == Key{} }

func ParseKey(s string) (Key, error) {
	var k Key
	t := strings.TrimSpace(s)
	switch {
	case len(t) == 2*KeyLen:
		b, err := hex.DecodeString(t)
		if err != nil {
			return k, fmt.Errorf("vpn: parse key: not hex: %w", err)
		}
		copy(k[:], b)
	case len(t) == base64.StdEncoding.EncodedLen(KeyLen):
		b, err := base64.StdEncoding.DecodeString(t)
		if err != nil {
			return k, fmt.Errorf("vpn: parse key: not base64: %w", err)
		}
		copy(k[:], b)
	default:
		return k, fmt.Errorf("vpn: parse key: want %d characters of base64 or %d of hex, got %d",
			base64.StdEncoding.EncodedLen(KeyLen), 2*KeyLen, len(t))
	}
	return k, nil
}

func ReadPrivateKey(path string) (Key, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Key{}, fmt.Errorf("vpn: read private key %s: %w", path, err)
	}
	k, err := ParseKey(string(b))
	if err != nil {
		return Key{}, fmt.Errorf("vpn: read private key %s: %w", path, err)
	}
	if k.IsZero() {
		return Key{}, fmt.Errorf("vpn: read private key %s: the key is empty", path)
	}
	return k.clamped(), nil
}

func WritePrivateKey(path string, k Key) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("vpn: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".private.key.*")
	if err != nil {
		return fmt.Errorf("vpn: create temp key file in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("vpn: chmod temp key file: %w", err)
	}
	if _, err := tmp.WriteString(k.Base64() + "\n"); err != nil {
		tmp.Close()
		return fmt.Errorf("vpn: write temp key file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("vpn: close temp key file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("vpn: install key file %s: %w", path, err)
	}
	return nil
}

func KeyFromSecret(secret string) Key {
	sum := sha256.Sum256([]byte("caramelo-join-v1:" + secret))
	return Key(sum).clamped()
}
