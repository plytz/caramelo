package vpn

import (
	"reflect"
	"testing"
)

func TestHosts(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"EnvHost", EnvHost("shop", "feat-x"), "feat-x.shop.internal"},
		{"ServiceHost", ServiceHost("shop", "feat-x", "db"), "db.feat-x.shop.internal"},
		{"MachineHost", MachineHost("worker1"), "worker1.internal"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestEnvHosts(t *testing.T) {
	got := EnvHosts("shop", "feat-x", []string{"web", "db"})
	want := []string{"feat-x.shop.internal", "web.feat-x.shop.internal", "db.feat-x.shop.internal"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("EnvHosts = %v, want %v", got, want)
	}
	if got := EnvHosts("shop", "feat-x", nil); !reflect.DeepEqual(got, want[:1]) {
		t.Errorf("EnvHosts with no services = %v, want %v", got, want[:1])
	}
}

func TestNormalizeAndIsInternal(t *testing.T) {
	tests := []struct {
		in       string
		want     string
		internal bool
	}{
		{"FEAT-X.Shop.Internal.", "feat-x.shop.internal", true},
		{" db.feat-x.shop.internal ", "db.feat-x.shop.internal", true},
		{"worker1.internal", "worker1.internal", true},

		{"internal", "internal", false},
		{"internal.", "internal", false},

		{"example.com", "example.com", false},
		{"feat-x.shop.internal.example.com", "feat-x.shop.internal.example.com", false},
		{"", "", false},
	}
	for _, tc := range tests {
		if got := Normalize(tc.in); got != tc.want {
			t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if got := IsInternal(tc.in); got != tc.internal {
			t.Errorf("IsInternal(%q) = %v, want %v", tc.in, got, tc.internal)
		}
	}
}

func TestProtocol(t *testing.T) {
	for _, p := range []Protocol{TCP, UDP} {
		if !p.Valid() {
			t.Errorf("%q is not valid", p)
		}
		if p.Network() != string(p) {
			t.Errorf("%q.Network() = %q", p, p.Network())
		}
	}
	if Protocol("sctp").Valid() {
		t.Error("sctp is valid, want it refused")
	}
}
