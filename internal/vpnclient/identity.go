package vpnclient

import (
	"path/filepath"

	"github.com/plytz/caramelo/internal/remote"
)

const IdentityKeyFile = "identity.key"

const identityOwner = "this commander"

func IdentityKeyPath() (string, error) {
	dir, err := remote.CommanderDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, IdentityKeyFile), nil
}

func EnsureIdentity() (KeyPair, bool, error) {
	path, err := IdentityKeyPath()
	if err != nil {
		return KeyPair{}, false, err
	}
	return ensureKeyFile(path, identityOwner)
}

func LoadIdentity() (KeyPair, error) {
	path, err := IdentityKeyPath()
	if err != nil {
		return KeyPair{}, err
	}
	return loadKeyFile(path, identityOwner)
}
