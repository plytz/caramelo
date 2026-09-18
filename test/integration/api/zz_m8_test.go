//go:build integration

package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	capi "github.com/plytz/caramelo/internal/api"
	cfleet "github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/test/integration/itest"
)

func TestZZAMachineOfOneIsAHub(t *testing.T) {
	_, o := box(t)

	res := itest.SSHAPIRun(t, o, "member", "list", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("member list: exit %d\nstdout:%s\nstderr:%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	var list []cfleet.Machine
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &list); err != nil {
		t.Fatalf("member list --json: %v\nstdout: %q", err, res.Stdout)
	}
	if len(list) != 1 {
		t.Fatalf("member list = %d rows on a machine of one, want 1: %+v", len(list), list)
	}
	if !list[0].Role.IsHub() {
		t.Errorf("a machine nobody joined has role %q, want %q", list[0].Role, cfleet.RoleHub)
	}
	if !list[0].Reachable(time.Now()) {
		t.Error("a machine reports itself unreachable; it is the one asking")
	}
	if got := list[0].Subnet.String(); got != cfleet.HubSubnet {
		t.Errorf("the hub holds %s, want %s", got, cfleet.HubSubnet)
	}

	named := itest.SSHAPIRun(t, o, "member", "show", list[0].Name, "--json")
	if named.ExitCode != 0 {
		t.Fatalf("member show %s: exit %d\nstderr:%s", list[0].Name, named.ExitCode, named.Stderr)
	}
	var detail capi.MachineDetail
	if err := json.Unmarshal([]byte(strings.TrimSpace(named.Stdout)), &detail); err != nil {
		t.Fatalf("member show %s --json: %v\nstdout: %q", list[0].Name, err, named.Stdout)
	}
	if detail.Machine.Name != list[0].Name {
		t.Errorf("member show %s answered about %q", list[0].Name, detail.Machine.Name)
	}
	if detail.Gauge == nil {
		t.Error("member show carries no gauge; placement has nothing to read")
	}
	if detail.Unreachable {
		t.Error("a machine says it cannot reach itself")
	}

	missing := itest.SSHAPIRun(t, o, "member", "show", "no-such-machine", "--json")
	if missing.ExitCode == 0 {
		t.Error("member show of a machine that does not exist succeeded")
	}
	if !strings.Contains(missing.Stdout+missing.Stderr, "no-such-machine") {
		t.Errorf("the refusal does not name what was asked for:\n%s%s", missing.Stdout, missing.Stderr)
	}
}

func TestZZAJoinTokenOverBothTransports(t *testing.T) {
	m, o := box(t)

	overSSH := itest.SSHAPIRun(t, o, "member", "token", "--json")
	if overSSH.ExitCode != 0 {
		t.Fatalf("member token over ssh: exit %d\nstdout:%s\nstderr:%s",
			overSSH.ExitCode, overSSH.Stdout, overSSH.Stderr)
	}
	first := decodeToken(t, "member token over ssh", overSSH.Stdout)
	if strings.TrimSpace(first.Token) == "" {
		t.Fatal("member token printed no token")
	}
	if first.Endpoint == "" {
		t.Error("member token carries no endpoint; a joining machine has nothing to dial")
	}
	if first.PublicKey == "" {
		t.Error("member token carries no public key; a joining machine has nothing to verify against")
	}
	if !first.ExpiresAt.After(time.Now()) {
		t.Errorf("the token expires at %v, which is not in the future", first.ExpiresAt)
	}

	local := itest.MustRunAsUser(t, m, itest.CarameloUser,
		itest.CarameloBinary+" member token --json")
	second := decodeToken(t, "member token over the socket", local.Stdout)
	if second.Token == first.Token {
		t.Error("two calls to `member token` produced the same token; each is for one machine")
	}

	for _, tok := range []string{first.Token, second.Token} {
		grep := itest.MustRunAsUser(t, m, "root",
			"grep -c -a -F "+itest.ShellQuote(tok)+" /var/lib/caramelo/caramelo.db || true")
		if got := strings.TrimSpace(grep.Stdout); got != "0" {
			t.Errorf("the state database holds a token's plaintext (%s match(es)); only its hash may be stored", got)
		}
	}

	long := itest.SSHAPIRun(t, o, "member", "token", "--ttl", "168h", "--json")
	if long.ExitCode == 0 {
		t.Error("`member token --ttl 168h` was accepted; the ceiling is a day")
	}
}

func decodeToken(t *testing.T, what, stdout string) capi.MachineTokenResult {
	t.Helper()
	var tok capi.MachineTokenResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &tok); err != nil {
		t.Fatalf("%s --json: %v\nstdout: %q", what, err, stdout)
	}
	return tok
}

func TestZZSecretsOnAMachineOfOneAreItsOwn(t *testing.T) {
	m, _ := box(t)
	res := itest.MustRunAsUser(t, m, itest.CarameloUser,
		itest.CarameloBinary+" secrets list --machine-scope --json")
	if res.ExitCode != 0 {
		t.Fatalf("secrets list on a machine of one: exit %d\nstdout:%s\nstderr:%s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
}
