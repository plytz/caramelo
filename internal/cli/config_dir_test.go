package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/serverconfig"
)

func configDirFlags(root *cobra.Command) map[string]string {
	out := map[string]string{}
	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		name := strings.TrimSpace(path + " " + c.Name())
		if f := c.Flags().Lookup("config-dir"); f != nil {
			out[name] = f.DefValue
		}
		for _, sub := range c.Commands() {
			walk(sub, name)
		}
	}
	walk(root, "")
	return out
}

func TestEveryConfigDirFlagFollowsTheEnvironment(t *testing.T) {
	t.Setenv(serverconfig.ConfigDirEnv, "")
	defaults := configDirFlags(manualRoot(t))
	for _, want := range []string{
		"caramelo hub setup", "caramelo hub status", "caramelo hub uninstall", "caramelo hub run",
		"caramelo member join", "caramelo member leave",
		"caramelo edge", "caramelo edge enable", "caramelo edge disable",
	} {
		if _, ok := defaults[want]; !ok {
			t.Errorf("%s has no --config-dir", want)
		}
	}
	for name, def := range defaults {
		if def != serverconfig.DefaultConfigDir {
			t.Errorf("%s defaults its --config-dir to %q, want %q", name, def, serverconfig.DefaultConfigDir)
		}
	}

	t.Setenv(serverconfig.ConfigDirEnv, "/opt/caramelo/etc")
	for name, def := range configDirFlags(manualRoot(t)) {
		if def != "/opt/caramelo/etc" {
			t.Errorf("%s defaults its --config-dir to %q although %s says /opt/caramelo/etc",
				name, def, serverconfig.ConfigDirEnv)
		}
	}
}
