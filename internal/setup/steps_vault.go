package setup

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/vault"
)

type VaultStep struct{}

func NewVaultStep() *VaultStep { return &VaultStep{} }

func (s *VaultStep) Name() string { return "vault" }

func (s *VaultStep) Check(ctx context.Context, env *Env) (bool, string, error) {
	cfg := env.Config
	path := cfg.VaultKeyPath()
	st, err := statPath(ctx, env, path)
	if err != nil {
		return false, "", err
	}
	switch {
	case !st.Exists:
		return false, path + " missing", nil
	case st.Owner != cfg.User || st.Group != cfg.Group:
		return false, fmt.Sprintf("%s owned by %s:%s, want %s:%s",
			path, st.Owner, st.Group, cfg.User, cfg.Group), nil
	case st.Mode != vaultKeyMode:
		return false, fmt.Sprintf("%s mode %s, want %s", path, st.Mode, vaultKeyMode), nil
	}
	if err := s.readKey(ctx, env); err != nil {

		return false, path + " is not a key", nil
	}
	return true, fmt.Sprintf("%s, %d-byte key", path, vault.KeySize), nil
}

const vaultKeyMode = "0600"

func (s *VaultStep) Apply(ctx context.Context, env *Env) error {
	cfg := env.Config
	path := cfg.VaultKeyPath()

	if err := s.readKey(ctx, env); err == nil {
		return s.own(ctx, env, path)
	}

	key, err := generateVaultKey()
	if err != nil {
		return err
	}

	spec := fileSpec{Path: path, Content: key + "\n", Mode: vaultKeyMode,
		Owner: cfg.User, Group: cfg.Group, AsUser: cfg.User}
	if err := spec.apply(ctx, env); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	logf(env, "generated the machine's vault key at %s", path)
	return nil
}

func (s *VaultStep) own(ctx context.Context, env *Env, path string) error {
	cfg := env.Config
	if _, err := mustRun(ctx, env, runner.Cmd{Name: "chown",
		Args: []string{cfg.User + ":" + cfg.Group, "--", path}}); err != nil {
		return err
	}
	_, err := mustRun(ctx, env, runner.Cmd{Name: "chmod", Args: []string{vaultKeyMode, "--", path}})
	return err
}

func (s *VaultStep) readKey(ctx context.Context, env *Env) error {
	content, exists, err := readFile(ctx, env, env.Config.VaultKeyPath())
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("no key at %s", env.Config.VaultKeyPath())
	}
	return validVaultKey(content)
}

func generateVaultKey() (string, error) {
	b := make([]byte, vault.KeySize)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate the vault key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func validVaultKey(content string) error {
	b, err := base64.StdEncoding.DecodeString(trimKey(content))
	if err != nil {
		return fmt.Errorf("the vault key is not base64")
	}
	if len(b) != vault.KeySize {
		return fmt.Errorf("the vault key is %d bytes, want %d", len(b), vault.KeySize)
	}
	return nil
}

func trimKey(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
		default:
			out = append(out, s[i])
		}
	}
	return string(out)
}
