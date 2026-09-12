//go:build integration

package itest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

const (
	LabKeyName    = "lab_key"
	LabKeyComment = "caramelo-itest"
)

var labKeyOnce sync.Once

type labKeyPaths struct {
	private string
	public  string
	err     error
}

var labKey labKeyPaths

func LabSSHKey() (private, public string, err error) {
	labKeyOnce.Do(func() { labKey = generateLabKey() })
	return labKey.private, labKey.public, labKey.err
}

func generateLabKey() labKeyPaths {
	dir, err := SharedCacheDir("ssh")
	if err != nil {
		return labKeyPaths{err: err}
	}
	lock, err := lockCache("ssh-key")
	if err != nil {
		return labKeyPaths{err: err}
	}
	defer lock.release()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return labKeyPaths{err: fmt.Errorf("create %s: %w", dir, err)}
	}
	priv := filepath.Join(dir, LabKeyName)
	pub := priv + ".pub"
	if fileExists(priv) && fileExists(pub) {
		return labKeyPaths{private: priv, public: pub}
	}
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return labKeyPaths{err: fmt.Errorf("generate the lab ssh key: %w", err)}
	}
	block, err := ssh.MarshalPrivateKey(privKey, LabKeyComment)
	if err != nil {
		return labKeyPaths{err: fmt.Errorf("marshal the lab ssh key: %w", err)}
	}
	if err := os.WriteFile(priv, pem.EncodeToMemory(block), 0o600); err != nil {
		return labKeyPaths{err: fmt.Errorf("write %s: %w", priv, err)}
	}
	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return labKeyPaths{err: fmt.Errorf("marshal the lab ssh public key: %w", err)}
	}
	if err := os.WriteFile(pub, ssh.MarshalAuthorizedKey(sshPub), 0o644); err != nil {
		return labKeyPaths{err: fmt.Errorf("write %s: %w", pub, err)}
	}
	return labKeyPaths{private: priv, public: pub}
}

func installLabKeys(ctx context.Context, m *Machine) error {
	priv, pub, err := LabSSHKey()
	if err != nil {
		return err
	}
	user := m.User()
	sshDir := m.Home() + "/.ssh"
	mkdir := "install -d -m 0700 -o " + user + " -g \"$(id -gn " + user + ")\" " + ShellQuote(sshDir)
	if res, err := m.RunAsRoot(ctx, mkdir); err != nil {
		return err
	} else if res.ExitCode != 0 {
		return fmt.Errorf("create %s on %s: %s", sshDir, m.Alias, res.Stderr)
	}
	switch m.Kind {
	case KindMachine:
		return authorizeLabKey(ctx, m, pub, sshDir+"/authorized_keys")
	case KindLaptop:
		if err := m.Copy(ctx, priv, sshDir+"/id_ed25519"); err != nil {
			return err
		}
		if err := chmodOn(ctx, m, "0600", sshDir+"/id_ed25519"); err != nil {
			return err
		}
		cfg := "Host *\n  IdentityFile " + sshDir + "/id_ed25519\n  IdentitiesOnly yes\n" +
			"  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null\n  LogLevel ERROR\n  BatchMode yes\n"
		if res, err := m.Run(ctx, "cat > "+sshDir+"/config <<'ITESTEOF'\n"+cfg+"ITESTEOF\nchmod 0600 "+sshDir+"/config"); err != nil {
			return err
		} else if res.ExitCode != 0 {
			return fmt.Errorf("write the ssh config on %s: %s", m.Alias, res.Stderr)
		}
		return nil
	}
	return nil
}

func authorizeLabKey(ctx context.Context, m *Machine, pub, authorized string) error {
	staged := authorized + ".itest"
	if err := m.Copy(ctx, pub, staged); err != nil {
		return err
	}
	f, s := ShellQuote(authorized), ShellQuote(staged)
	merge := fmt.Sprintf("touch %s && { grep -qxFf %s %s || { [ -s %s ] && [ -n \"$(tail -c1 %s)\" ] && echo >> %s; cat %s >> %s; }; } "+
		"&& chmod 0600 %s && rm -f %s", f, s, f, f, f, f, s, f, f, s)
	res, err := m.Run(ctx, merge)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("authorize the lab key on %s: exit %d: %s", m.Alias, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func chmodOn(ctx context.Context, m *Machine, mode, path string) error {
	res, err := m.RunAsRoot(ctx, "chmod "+mode+" "+ShellQuote(path))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("chmod %s %s on %s: %s", mode, path, m.Alias, res.Stderr)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
