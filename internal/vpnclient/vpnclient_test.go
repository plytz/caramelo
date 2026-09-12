package vpnclient

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyFileName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"worker1", "worker1.key"},
		{"caramelo@192.168.56.11:4022", "caramelo-192.168.56.11-4022.key"},
		{"../../etc/passwd", "..-..-etc-passwd.key"},
		{"", "machine.key"},
		{".", "machine.key"},
		{"..", "machine.key"},
	}
	for _, tc := range tests {
		got := KeyFileName(tc.in)
		if got != tc.want {
			t.Errorf("KeyFileName(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("KeyFileName(%q) = %q contains a path separator", tc.in, got)
		}
		if base := filepath.Base(got); base != got {
			t.Errorf("KeyFileName(%q) = %q is not a single path element", tc.in, got)
		}
	}
}

func TestKeyPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/home/someone/.config")
	got, err := KeyPath("worker1")
	if err != nil {
		t.Fatal(err)
	}
	if want := "/home/someone/.config/caramelo/vpn/worker1.key"; got != want {
		t.Errorf("KeyPath = %q, want %q", got, want)
	}
}
