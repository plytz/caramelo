package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type aeadCipher struct{ aead cipher.AEAD }

var _ Cipher = (*aeadCipher)(nil)

func NewCipher(key []byte) (Cipher, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("the vault key is %d bytes, want %d (AES-256): %w",
			len(key), KeySize, ErrNoKey)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("vault cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("vault cipher: %w", err)
	}
	return &aeadCipher{aead: aead}, nil
}

func (c *aeadCipher) Seal(plaintext []byte) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("vault: read a nonce: %w", err)
	}
	return c.aead.Seal(nil, nonce, plaintext, nil), nonce, nil
}

func (c *aeadCipher) Open(ciphertext, nonce []byte) ([]byte, error) {
	if len(nonce) != c.aead.NonceSize() {
		return nil, fmt.Errorf("vault: the nonce is %d bytes, want %d", len(nonce), c.aead.NonceSize())
	}
	out, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("vault: this value cannot be decrypted with the machine's key "+
			"(a restored database with a different vault.key, or a damaged row): %w", err)
	}
	return out, nil
}

func LoadKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("no vault key at %s "+
			"(run `caramelo fleet setup` again to create it): %w", path, ErrNoKey)
	case err != nil:
		return nil, fmt.Errorf("read the vault key at %s: %w", path, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read the vault key at %s: %w", path, err)
	}
	if err := checkKeyMode(path, st.Mode().Perm()); err != nil {
		return nil, err
	}
	return decodeKey(path, string(raw))
}

func checkKeyMode(path string, perm fs.FileMode) error {
	if perm&0o077 == 0 {
		return nil
	}
	return fmt.Errorf("the vault key at %s is mode %04o, which lets somebody other than its owner "+
		"read every secret on this machine: run `chmod %04o %s`: %w",
		path, perm, fs.FileMode(KeyMode).Perm(), path, ErrNoKey)
}

func decodeKey(path, content string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(content))
	if err != nil {
		return nil, fmt.Errorf("the vault key at %s is not base64 "+
			"(it is written by `caramelo fleet setup`; do not edit it): %w", path, ErrNoKey)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("the vault key at %s is %d bytes, want %d: %w",
			path, len(key), KeySize, ErrNoKey)
	}
	return key, nil
}

func NewKey() (string, error) {
	b := make([]byte, KeySize)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate a vault key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func EnsureKey(path string) (key []byte, created bool, err error) {
	if _, statErr := os.Stat(path); statErr == nil {

		k, err := LoadKey(path)
		return k, false, err
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return nil, false, fmt.Errorf("read the vault key at %s: %w", path, statErr)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("create the directory for the vault key: %w", err)
	}
	encoded, err := NewKey()
	if err != nil {
		return nil, false, err
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, KeyMode)
	if errors.Is(err, fs.ErrExist) {
		k, err := LoadKey(path)
		return k, false, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("create the vault key at %s: %w", path, err)
	}
	if _, err := f.WriteString(encoded + "\n"); err != nil {
		f.Close()
		return nil, false, fmt.Errorf("write the vault key at %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, false, fmt.Errorf("write the vault key at %s: %w", path, err)
	}
	k, err := decodeKey(path, encoded)
	if err != nil {
		return nil, false, err
	}
	return k, true, nil
}

func CipherFromKeyFile(path string) (Cipher, bool, error) {
	key, created, err := EnsureKey(path)
	if err != nil {
		return nil, false, err
	}
	c, err := NewCipher(key)
	if err != nil {
		return nil, created, err
	}
	return c, created, nil
}
