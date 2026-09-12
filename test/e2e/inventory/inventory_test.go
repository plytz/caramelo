//go:build e2e

package inventory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeKey(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("not-a-real-key\n"), 0o600); err != nil {
		t.Fatalf("writing key fixture: %v", err)
	}
	return path
}

func writeInventory(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "inventory.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing inventory fixture: %v", err)
	}
	return path
}

func TestLoadValidThreeMachines(t *testing.T) {
	dir := t.TempDir()
	key := writeKey(t, dir, "id_ed25519")
	path := writeInventory(t, dir, `{
  "version": 1,
  "machines": [
    {"name": "m1", "host": "198.51.100.10", "port": 22, "user": "debian", "key": "`+key+`", "arch": "amd64", "host_key": "ssh-ed25519 QUFBQQ=="},
    {"name": "m2", "host": "198.51.100.11", "user": "debian", "key": "`+key+`", "arch": "arm64"},
    {"name": "m3", "host": "198.51.100.12", "port": 2222, "user": "ubuntu", "key": "`+key+`", "arch": "amd64"}
  ]
}`)

	inv, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if inv.Version != 1 {
		t.Errorf("version = %d, want 1", inv.Version)
	}
	if inv.Count() != 3 {
		t.Fatalf("Count() = %d, want 3", inv.Count())
	}
	hub, ok := inv.Hub()
	if !ok {
		t.Fatal("Hub() reported no hub")
	}
	if hub.Name != "m1" || hub.Host != "198.51.100.10" || hub.Port != 22 || hub.User != "debian" || hub.Arch != "amd64" {
		t.Errorf("Hub() = %+v", hub)
	}
	if hub.Key != key {
		t.Errorf("Hub().Key = %q, want %q", hub.Key, key)
	}
	if hub.HostKey != "ssh-ed25519 QUFBQQ==" {
		t.Errorf("Hub().HostKey = %q", hub.HostKey)
	}
	nodes := inv.Nodes()
	if len(nodes) != 2 {
		t.Fatalf("len(Nodes()) = %d, want 2", len(nodes))
	}
	if nodes[0].Name != "m2" || nodes[1].Name != "m3" {
		t.Errorf("Nodes() = %+v", nodes)
	}
	if nodes[1].Port != 2222 {
		t.Errorf("Nodes()[1].Port = %d, want 2222", nodes[1].Port)
	}
	if nodes[0].HostKey != "" {
		t.Errorf("Nodes()[0].HostKey = %q, want empty", nodes[0].HostKey)
	}
}

func TestLoadDefaultsPortTo22(t *testing.T) {
	dir := t.TempDir()
	key := writeKey(t, dir, "id_ed25519")
	path := writeInventory(t, dir, `{"version":1,"machines":[{"name":"m1","host":"h","user":"debian","key":"`+key+`","arch":"amd64"}]}`)
	inv, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := inv.Machines[0].Port; got != DefaultPort {
		t.Errorf("port = %d, want %d", got, DefaultPort)
	}
}

func TestLoadExpandsTildeInKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	key := writeKey(t, filepath.Join(home, ".ssh"), "caramelo-e2e")

	dir := t.TempDir()
	path := writeInventory(t, dir, `{"version":1,"machines":[{"name":"m1","host":"h","user":"debian","key":"~/.ssh/caramelo-e2e","arch":"amd64"}]}`)
	inv, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := inv.Machines[0].Key; got != key {
		t.Errorf("key = %q, want %q", got, key)
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	key := writeKey(t, dir, "id_ed25519")

	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "version 2",
			body: `{"version":2,"machines":[]}`,
			want: []string{"version 2", "version 1"},
		},
		{
			name: "version missing",
			body: `{"machines":[]}`,
			want: []string{"version 0"},
		},
		{
			name: "unknown field",
			body: `{"version":1,"machines":[{"name":"m1","host":"h","user":"debian","key":"` + key + `","hostkey":"ssh-ed25519 QUFBQQ=="}]}`,
			want: []string{`unknown field "hostkey"`},
		},
		{
			name: "unknown top level field",
			body: `{"version":1,"machine":[]}`,
			want: []string{`unknown field "machine"`},
		},
		{
			name: "duplicate names",
			body: `{"version":1,"machines":[{"name":"m1","host":"h1","user":"u","key":"` + key + `"},{"name":"m1","host":"h2","user":"u","key":"` + key + `"}]}`,
			want: []string{`named "m1"`, "unique"},
		},
		{
			name: "missing name",
			body: `{"version":1,"machines":[{"host":"h","user":"u","key":"` + key + `"}]}`,
			want: []string{"machines[0] has no name"},
		},
		{
			name: "missing host",
			body: `{"version":1,"machines":[{"name":"m1","user":"u","key":"` + key + `"}]}`,
			want: []string{"machine m1 has no host"},
		},
		{
			name: "missing user",
			body: `{"version":1,"machines":[{"name":"m1","host":"h","key":"` + key + `"}]}`,
			want: []string{"machine m1 has no user"},
		},
		{
			name: "missing key",
			body: `{"version":1,"machines":[{"name":"m1","host":"h","user":"u"}]}`,
			want: []string{"machine m1 has no key"},
		},
		{
			name: "key file absent",
			body: `{"version":1,"machines":[{"name":"m1","host":"h","user":"u","key":"` + filepath.Join(dir, "nope") + `"}]}`,
			want: []string{"machine m1 key", "nope", "does not exist"},
		},
		{
			name: "port out of range",
			body: `{"version":1,"machines":[{"name":"m1","host":"h","user":"u","key":"` + key + `","port":70000}]}`,
			want: []string{"machine m1 has port 70000"},
		},
		{
			name: "host_key one field",
			body: `{"version":1,"machines":[{"name":"m1","host":"h","user":"u","key":"` + key + `","host_key":"AAAAC3Nz"}]}`,
			want: []string{"machine m1 has host_key", "<type> <base64>"},
		},
		{
			name: "host_key not base64",
			body: `{"version":1,"machines":[{"name":"m1","host":"h","user":"u","key":"` + key + `","host_key":"ssh-ed25519 not base64 at all"}]}`,
			want: []string{"machine m1 has host_key", "<type> <base64>"},
		},
		{
			name: "not json",
			body: `this is not json`,
			want: []string{"is not a valid inventory"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeInventory(t, t.TempDir(), tc.body)
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load succeeded, want an error")
			}
			msg := err.Error()
			if !strings.Contains(msg, path) {
				t.Errorf("error %q does not name the inventory file %s", msg, path)
			}
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not contain %q", msg, want)
				}
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load succeeded, want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, path) || !strings.Contains(msg, EnvVar) {
		t.Errorf("error %q must name both the file and %s", msg, EnvVar)
	}
}

func TestLoadEnv(t *testing.T) {
	dir := t.TempDir()
	key := writeKey(t, dir, "id_ed25519")
	path := writeInventory(t, dir, `{"version":1,"machines":[{"name":"m1","host":"h","user":"u","key":"`+key+`","arch":"amd64"}]}`)

	t.Setenv(EnvVar, path)
	inv, got, err := LoadEnv()
	if err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	if got != path {
		t.Errorf("LoadEnv path = %q, want %q", got, path)
	}
	if inv.Count() != 1 {
		t.Errorf("Count() = %d, want 1", inv.Count())
	}
}

func TestLoadEnvUnset(t *testing.T) {
	t.Setenv(EnvVar, "")
	_, path, err := LoadEnv()
	if err == nil {
		t.Fatal("LoadEnv succeeded, want an error")
	}
	if path != "" {
		t.Errorf("path = %q, want empty", path)
	}
	if !strings.Contains(err.Error(), EnvVar) {
		t.Errorf("error %q does not name %s", err.Error(), EnvVar)
	}
}

func TestRolesOnEmptyOneAndThree(t *testing.T) {
	cases := []struct {
		name      string
		inv       Inventory
		count     int
		wantHub   bool
		hubName   string
		wantNodes []string
	}{
		{name: "zero machines", inv: Inventory{Version: 1}, count: 0, wantHub: false},
		{
			name:    "one machine",
			inv:     Inventory{Version: 1, Machines: []Machine{{Name: "m1"}}},
			count:   1,
			wantHub: true,
			hubName: "m1",
		},
		{
			name:      "three machines",
			inv:       Inventory{Version: 1, Machines: []Machine{{Name: "m1"}, {Name: "m2"}, {Name: "m3"}}},
			count:     3,
			wantHub:   true,
			hubName:   "m1",
			wantNodes: []string{"m2", "m3"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.inv.Count(); got != tc.count {
				t.Errorf("Count() = %d, want %d", got, tc.count)
			}
			hub, ok := tc.inv.Hub()
			if ok != tc.wantHub {
				t.Fatalf("Hub() ok = %v, want %v", ok, tc.wantHub)
			}
			if ok && hub.Name != tc.hubName {
				t.Errorf("Hub().Name = %q, want %q", hub.Name, tc.hubName)
			}
			nodes := tc.inv.Nodes()
			if len(nodes) != len(tc.wantNodes) {
				t.Fatalf("len(Nodes()) = %d, want %d", len(nodes), len(tc.wantNodes))
			}
			for i, want := range tc.wantNodes {
				if nodes[i].Name != want {
					t.Errorf("Nodes()[%d].Name = %q, want %q", i, nodes[i].Name, want)
				}
			}
		})
	}
}

func TestNodesDoesNotAliasMachines(t *testing.T) {
	inv := Inventory{Version: 1, Machines: []Machine{{Name: "m1"}, {Name: "m2"}}}
	nodes := inv.Nodes()
	nodes[0].Name = "changed"
	if inv.Machines[1].Name != "m2" {
		t.Errorf("Nodes() aliases Machines: machines[1].Name = %q", inv.Machines[1].Name)
	}
}
