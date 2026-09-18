package serverconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plytz/caramelo/internal/userdir"
)

func TestAServerConfigTurnsOffTheSudoRule(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ConfigDirEnv, dir)
	if userdir.ServerConfigured() {
		t.Fatal("an empty directory reported a server config")
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte("name: box\nrole: hub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !userdir.ServerConfigured() {
		t.Error("a written config.yaml did not report a server config")
	}
}
