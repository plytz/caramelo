package setup

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/setup/testutil"
	"github.com/plytz/caramelo/internal/vault"
)

const vaultKeyPath = "/var/lib/caramelo/vault.key"

func TestVaultCheckOnABareBox(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix("stat -c %U:%G:%a:%F -- ", 1)
	env, _ := testEnv(t, run)

	done, detail, err := (&VaultStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if done || !strings.Contains(detail, vaultKeyPath+" missing") {
		t.Fatalf("Check() = %v, %q; want the missing key", done, detail)
	}
}

func TestVaultApplyGeneratesTheKey(t *testing.T) {
	run := testutil.New()
	run.ExitPrefix("cat -- ", 1)
	env, log := testEnv(t, run)

	if err := (&VaultStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v\n%s", err, run.Transcript())
	}

	var written string
	for _, c := range run.Calls() {
		if c.Line == "tee -- "+vaultKeyPath {
			written = c.Stdin
			if c.Cmd.User != "caramelo" {
				t.Errorf("the key was written as %q, want the daemon's user", c.Cmd.User)
			}
		}
	}
	if written == "" {
		t.Fatalf("the key was never written:\n%s", run.Transcript())
	}
	if err := validVaultKey(written); err != nil {
		t.Errorf("what was written is not a key: %v", err)
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(written))
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != vault.KeySize {
		t.Errorf("the key is %d bytes, want %d", len(b), vault.KeySize)
	}

	if !run.Ran("chmod 0600 -- "+vaultKeyPath) || !run.Ran("chown caramelo:caramelo -- "+vaultKeyPath) {
		t.Errorf("the key is not the daemon's and 0600 only:\n%s", run.Transcript())
	}

	if strings.Contains(log.String(), strings.TrimSpace(written)) {
		t.Error("the vault key was printed")
	}
	if !strings.Contains(log.String(), vaultKeyPath) {
		t.Errorf("the step did not say what it made:\n%s", log.String())
	}
}

func TestVaultApplyNeverReplacesAnExistingKey(t *testing.T) {
	run := testutil.New()
	key, err := generateVaultKey()
	if err != nil {
		t.Fatal(err)
	}
	run.Stdout("cat -- "+vaultKeyPath, key+"\n")
	env, _ := testEnv(t, run)

	if err := (&VaultStep{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply() error: %v\n%s", err, run.Transcript())
	}
	if run.Ran("tee -- " + vaultKeyPath) {
		t.Errorf("the key was rewritten:\n%s", run.Transcript())
	}

	if !run.Ran("chmod 0600 -- "+vaultKeyPath) || !run.Ran("chown caramelo:caramelo -- "+vaultKeyPath) {
		t.Errorf("the key's owner and mode were not corrected:\n%s", run.Transcript())
	}
}

func TestVaultCheckIsDoneOnAFinishedBox(t *testing.T) {
	run := testutil.New()
	key, err := generateVaultKey()
	if err != nil {
		t.Fatal(err)
	}
	run.Stdout("stat -c %U:%G:%a:%F -- "+vaultKeyPath, "caramelo:caramelo:600:regular file\n")
	run.Stdout("cat -- "+vaultKeyPath, key+"\n")
	env, _ := testEnv(t, run)

	done, detail, err := (&VaultStep{}).Check(context.Background(), env)
	if err != nil || !done {
		t.Fatalf("Check() = %v, %q, %v; want done", done, detail, err)
	}
	if !strings.Contains(detail, vaultKeyPath) {
		t.Errorf("Check() detail = %q", detail)
	}
	if strings.Contains(detail, strings.TrimSpace(key)) {
		t.Error("the detail quotes the key")
	}
}

func TestVaultCheckRefusesRubbish(t *testing.T) {
	for _, content := range []string{
		"not base64 at all !!!",
		base64.StdEncoding.EncodeToString([]byte("too short")),
		"",
	} {
		run := testutil.New()
		run.Stdout("stat -c %U:%G:%a:%F -- "+vaultKeyPath, "caramelo:caramelo:600:regular file\n")
		run.Stdout("cat -- "+vaultKeyPath, content+"\n")
		env, _ := testEnv(t, run)

		done, detail, err := (&VaultStep{}).Check(context.Background(), env)
		if err != nil {
			t.Fatalf("Check() error: %v", err)
		}
		if done {
			t.Errorf("Check() said done for %q", content)
		}
		if !strings.Contains(detail, "is not a key") {
			t.Errorf("Check() detail = %q", detail)
		}
	}
}

func TestVaultCheckRefusesAWrongMode(t *testing.T) {
	run := testutil.New()
	run.Stdout("stat -c %U:%G:%a:%F -- "+vaultKeyPath, "caramelo:caramelo:644:regular file\n")
	env, _ := testEnv(t, run)

	done, detail, err := (&VaultStep{}).Check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if done || !strings.Contains(detail, "mode 0644") {
		t.Errorf("Check() = %v, %q", done, detail)
	}
}

func TestVaultKeyModeAgrees(t *testing.T) {
	if vaultKeyMode != "0600" || vault.KeyMode != 0o600 {
		t.Errorf("vaultKeyMode = %q, vault.KeyMode = %o", vaultKeyMode, vault.KeyMode)
	}
}

func TestValidVaultKeyTolerantOfWhitespace(t *testing.T) {
	key, err := generateVaultKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{key, key + "\n", " " + key + " \r\n", key[:10] + "\n" + key[10:]} {
		if err := validVaultKey(content); err != nil {
			t.Errorf("validVaultKey(%q…) = %v", content[:8], err)
		}
	}
}
