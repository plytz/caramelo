package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestContainerStateRunning(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{StatusRunning, true},
		{StatusExited, false},
		{StatusCreated, false},
		{"restarting", false},
		{"paused", false},
		{"dead", false},
		{"removing", false},

		{"Running", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("status=%q", tc.status), func(t *testing.T) {
			c := ContainerState{Name: "caramelo-shop-feat-db", Status: tc.status}
			if got := c.Running(); got != tc.want {
				t.Errorf("Running() = %v for status %q, want %v", got, tc.status, tc.want)
			}
		})
	}
}

func TestErrNotFound(t *testing.T) {

	wrapped := fmt.Errorf("inspect db: %w", ErrNotFound)
	if !errors.Is(wrapped, ErrNotFound) {
		t.Error("a wrapped ErrNotFound is not recognised by errors.Is")
	}
	if errors.Is(errors.New("not found"), ErrNotFound) {
		t.Error("a same-text error must not compare equal to ErrNotFound")
	}
}

func TestContainerStateJSON(t *testing.T) {
	full := ContainerState{
		Name:   "caramelo-shop-feat-db",
		ID:     "abc123",
		Status: StatusRunning,
		Image:  "postgres:16",
		Labels: map[string]string{"caramelo.env": "feat"},
		Ports:  []PortMap{{HostIP: "127.0.0.1", HostPort: 20000, ContainerPort: 5432}},
	}
	b, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"name":"caramelo-shop-feat-db","id":"abc123","status":"running","image":"postgres:16",` +
		`"labels":{"caramelo.env":"feat"},"ports":[{"host_ip":"127.0.0.1","host_port":20000,"container_port":5432}]}`
	if string(b) != want {
		t.Errorf("json =\n %s\nwant\n %s", b, want)
	}

	var back ContainerState
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !back.Running() || back.Name != full.Name || len(back.Ports) != 1 {
		t.Errorf("round trip = %+v, want %+v", back, full)
	}

	b, err = json.Marshal(ContainerState{Name: "n", Status: StatusExited})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"name":"n","id":"","status":"exited","image":""}`; string(b) != want {
		t.Errorf("json = %s, want %s", b, want)
	}
}

func TestVolumeMountJSON(t *testing.T) {
	b, err := json.Marshal(VolumeMount{Volume: "caramelo-shop-feat-db", Path: "/var/lib/postgresql/data"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if want := `{"volume":"caramelo-shop-feat-db","path":"/var/lib/postgresql/data"}`; string(b) != want {
		t.Errorf("json = %s, want %s", b, want)
	}
	b, err = json.Marshal(VolumeMount{Volume: "v", Path: "/p", ReadOnly: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"volume":"v","path":"/p","read_only":true}`; string(b) != want {
		t.Errorf("json = %s, want %s", b, want)
	}
}
