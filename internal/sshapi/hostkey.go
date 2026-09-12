package sshapi

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	gossh "golang.org/x/crypto/ssh"
)

func EnsureHostKey(path string) (gossh.Signer, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		signer, err := gossh.ParsePrivateKey(b)
		if err != nil {
			return nil, fmt.Errorf("parse host key %s: %w", path, err)
		}
		if err := writePublicKey(path+".pub", signer); err != nil {
			return nil, err
		}
		return signer, nil
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, fmt.Errorf("read host key %s: %w", path, err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate host key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal host key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := writeFileAtomic(path, pemBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write host key %s: %w", path, err)
	}
	signer, err := gossh.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, fmt.Errorf("parse generated host key: %w", err)
	}
	if err := writePublicKey(path+".pub", signer); err != nil {
		return nil, err
	}
	return signer, nil
}

func writePublicKey(path string, signer gossh.Signer) error {
	line := gossh.MarshalAuthorizedKey(signer.PublicKey())
	host, err := os.Hostname()
	if err == nil && host != "" {
		line = append(line[:len(line)-1], []byte(" caramelod@"+host+"\n")...)
	}
	if old, err := os.ReadFile(path); err == nil && string(old) == string(line) {
		return nil
	}
	if err := writeFileAtomic(path, line, 0o644); err != nil {
		return fmt.Errorf("write host public key %s: %w", path, err)
	}
	return nil
}

func Fingerprint(signer gossh.Signer) string {
	if signer == nil {
		return ""
	}
	return gossh.FingerprintSHA256(signer.PublicKey())
}

func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
