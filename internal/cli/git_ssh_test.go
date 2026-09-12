package cli

import (
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/serverconfig"
)

func TestParseSSHArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		user    string
		host    string
		port    int
		command string
	}{
		{
			name:    "what git runs for a push",
			args:    []string{"-p", "4022", "caramelo@worker1.internal", "git-receive-pack '/shop'"},
			user:    "caramelo",
			host:    "worker1.internal",
			port:    4022,
			command: "git-receive-pack '/shop'",
		},
		{
			name:    "no user and no port fall back to the daemon's",
			args:    []string{"worker1.internal", "git-upload-pack '/shop'"},
			user:    serverconfig.DefaultUser,
			host:    "worker1.internal",
			port:    serverconfig.DefaultSSHPort,
			command: "git-upload-pack '/shop'",
		},
		{
			name:    "an attached port value",
			args:    []string{"-p4022", "caramelo@box", "git-upload-pack '/shop'"},
			user:    "caramelo",
			host:    "box",
			port:    4022,
			command: "git-upload-pack '/shop'",
		},
		{
			name:    "options with values are skipped with them",
			args:    []string{"-o", "SendEnv=GIT_PROTOCOL", "-i", "/tmp/key", "box", "git-upload-pack '/shop'"},
			user:    serverconfig.DefaultUser,
			host:    "box",
			port:    serverconfig.DefaultSSHPort,
			command: "git-upload-pack '/shop'",
		},
		{
			name:    "switches are skipped",
			args:    []string{"-4", "-q", "-T", "box", "git-upload-pack '/shop'"},
			user:    serverconfig.DefaultUser,
			host:    "box",
			port:    serverconfig.DefaultSSHPort,
			command: "git-upload-pack '/shop'",
		},
		{
			name:    "-l names the user",
			args:    []string{"-l", "someone", "box", "git-upload-pack '/shop'"},
			user:    "someone",
			host:    "box",
			port:    serverconfig.DefaultSSHPort,
			command: "git-upload-pack '/shop'",
		},
		{
			name:    "the command is rejoined the way ssh joins it",
			args:    []string{"box", "git-upload-pack", "'/shop'"},
			user:    serverconfig.DefaultUser,
			host:    "box",
			port:    serverconfig.DefaultSSHPort,
			command: "git-upload-pack '/shop'",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv, err := parseSSHArgs(tc.args)
			if err != nil {
				t.Fatalf("parseSSHArgs(%q): %v", tc.args, err)
			}
			if inv.User != tc.user || inv.Host != tc.host || inv.Port != tc.port || inv.Command != tc.command {
				t.Errorf("parseSSHArgs(%q) = %+v, want user %q host %q port %d command %q",
					tc.args, inv, tc.user, tc.host, tc.port, tc.command)
			}
		})
	}
}

func TestParseSSHArgsRejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no command", []string{"box"}},
		{"no host", []string{"-p", "4022"}},
		{"a port that is not one", []string{"-p", "nope", "box", "git-upload-pack '/x'"}},
		{"a port out of range", []string{"-p", "70000", "box", "git-upload-pack '/x'"}},
		{"an option with no value", []string{"-p"}},
		{"an empty user", []string{"@box", "git-upload-pack '/x'"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if inv, err := parseSSHArgs(tc.args); err == nil {
				t.Fatalf("parseSSHArgs(%q) = %+v, want an error", tc.args, inv)
			}
		})
	}
}

func TestGitSSHCommand(t *testing.T) {
	if got, want := gitSSHCommandTunnel("/usr/local/bin/caramelo"), "/usr/local/bin/caramelo git-ssh"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got := gitSSHCommandTunnel("/opt/my tools/caramelo")
	if !strings.HasPrefix(got, "'/opt/my tools/caramelo'") {
		t.Errorf("a path with a space was not quoted: %q", got)
	}
	if !strings.HasSuffix(got, " git-ssh") {
		t.Errorf("got %q, want it to end in the subcommand", got)
	}
}

func TestGitSSHIsLocalAndHidden(t *testing.T) {
	root := NewRootCmd(&strings.Builder{}, &strings.Builder{})
	cmd, _, err := root.Find([]string{"git-ssh"})
	if err != nil {
		t.Fatal(err)
	}
	if !cmd.Hidden {
		t.Error("git-ssh is listed in help")
	}
	if isClient(cmd) {
		t.Error("git-ssh is marked as a client command; it would be forwarded through itself")
	}
	if !cmd.DisableFlagParsing {
		t.Error("cobra would parse ssh's flags as its own")
	}
}
