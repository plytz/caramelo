package vpnclient

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type FileKeyStore struct {
	Dir string
}

var _ KeyStore = (*FileKeyStore)(nil)

func (s *FileKeyStore) Path(machine string) string {
	if s.Dir != "" {
		return filepath.Join(s.Dir, KeyFileName(machine))
	}
	path, err := KeyPath(machine)
	if err != nil {

		return filepath.Join("caramelo", KeyDir, KeyFileName(machine))
	}
	return path
}

func (s *FileKeyStore) Ensure(machine string) (KeyPair, bool, error) {
	kp, err := s.Load(machine)
	switch {
	case err == nil:
		return kp, false, nil
	case !errors.Is(err, ErrNoKey):
		return KeyPair{}, false, err
	}
	kp, err = GenerateKey()
	if err != nil {
		return KeyPair{}, false, err
	}
	path := s.Path(machine)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return KeyPair{}, false, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		kp, err := s.Load(machine)
		return kp, false, err
	}
	if err != nil {
		return KeyPair{}, false, fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.WriteString(kp.Private + "\n"); err != nil {
		_ = f.Close()
		return KeyPair{}, false, fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return KeyPair{}, false, fmt.Errorf("write %s: %w", path, err)
	}
	return kp, true, nil
}

func (s *FileKeyStore) Load(machine string) (KeyPair, error) {
	path := s.Path(machine)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return KeyPair{}, fmt.Errorf("%s: %w", machine, ErrNoKey)
	}
	if err != nil {
		return KeyPair{}, fmt.Errorf("read %s: %w", path, err)
	}
	private := strings.TrimSpace(string(b))
	if private == "" {
		return KeyPair{}, fmt.Errorf("%s is empty; remove it and run 'caramelo vpn up' again", path)
	}
	public, err := PublicKey(private)
	if err != nil {
		return KeyPair{}, fmt.Errorf("%s: %w", path, err)
	}

	private, err = KeyBase64(private)
	if err != nil {
		return KeyPair{}, fmt.Errorf("%s: %w", path, err)
	}
	return KeyPair{Private: private, Public: public}, nil
}

func (s *FileKeyStore) Remove(machine string) error {
	path := s.Path(machine)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}
