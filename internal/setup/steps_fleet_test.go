package setup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/setup/testutil"
)

func aTicket(t *testing.T) (fleet.Ticket, string) {
	t.Helper()
	tk := fleet.Ticket{
		Hub:       "nx1",
		Endpoint:  "hub.example.com:4021",
		PublicKey: "Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9=",
		Address:   "10.86.0.1",
		Peer:      "10.86.0.9",
		Range:     fleet.FleetRange,
		Secret:    "s3cr3t",
	}
	s, err := tk.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return tk, s
}

func TestJoinStepSkipsWithoutATicket(t *testing.T) {
	env, _ := testEnv(t, testutil.New())
	_, _, err := NewJoinStep().Check(context.Background(), env)
	var skip Skip
	if !errors.As(err, &skip) {
		t.Fatalf("Check = %v, want a Skip", err)
	}
}

func TestJoinStepRunsTheJoinAnOperatorWouldRun(t *testing.T) {
	run := testutil.New()
	env, _ := testEnv(t, run)
	log := &strings.Builder{}
	env.Log = log
	tk, token := aTicket(t)
	env.Opts.Join = JoinSpec{Token: token, Name: "m1"}

	done, detail, err := NewJoinStep().Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("a machine that has joined nobody says it is a member")
	}
	if !strings.Contains(detail, tk.Hub) {
		t.Errorf("detail = %q, want it to name the hub", detail)
	}

	if err := NewJoinStep().Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	cmds := run.Lines()
	var joined string
	for _, c := range cmds {
		if strings.Contains(c, "machine join") {
			joined = c
		}
	}
	if joined == "" {
		t.Fatalf("no join was run; commands were %q", cmds)
	}
	for _, want := range []string{
		serverconfig.BinaryPath, "machine join", tk.Endpoint, "--token -", "--json", "--name m1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the join command %q does not carry %q", joined, want)
		}
	}

	if strings.Contains(joined, token) {
		t.Errorf("the join command carries the ticket in argv: %q", joined)
	}
	var fed string
	for _, c := range run.Calls() {
		if strings.Contains(c.Line, "machine join") {
			fed = c.Stdin
		}
	}
	if fed != token {
		t.Errorf("the join was fed %q on standard input, want the ticket", fed)
	}
	if !strings.Contains(log.String(), tk.Hub) {
		t.Errorf("the log %q does not say which hub was joined", log.String())
	}
}

func TestJoinStepCarriesPrivate(t *testing.T) {
	run := testutil.New()
	env, _ := testEnv(t, run)
	_, token := aTicket(t)
	env.Opts.Join = JoinSpec{Token: token}
	env.Config.Fleet.Private = true

	if err := NewJoinStep().Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var joined string
	for _, c := range run.Lines() {
		if strings.Contains(c, "machine join") {
			joined = c
		}
	}
	if !strings.Contains(joined, "--private") {
		t.Errorf("the join command %q does not carry --private", joined)
	}
}

func TestJoinStepIsDoneOnAMachineThatAlreadyJoined(t *testing.T) {
	env, _ := testEnv(t, testutil.New())
	tk, token := aTicket(t)
	env.Opts.Join = JoinSpec{Token: token}
	env.Config.Fleet = serverconfig.Fleet{
		Role: serverconfig.RoleMember,
		Hub: serverconfig.FleetHub{
			Name: tk.Hub, Endpoint: tk.Endpoint, Address: tk.Address, PublicKey: tk.PublicKey,
		},
	}
	done, detail, err := NewJoinStep().Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !done {
		t.Fatalf("a member of %s was asked to join it again", tk.Hub)
	}
	if !strings.Contains(detail, tk.Hub) {
		t.Errorf("detail = %q, want it to name the hub", detail)
	}
}

func TestJoinStepRefusesToMoveAMachineBetweenHubs(t *testing.T) {
	env, _ := testEnv(t, testutil.New())
	_, token := aTicket(t)
	env.Opts.Join = JoinSpec{Token: token}
	env.Config.Fleet = serverconfig.Fleet{
		Role: serverconfig.RoleMember,
		Hub:  serverconfig.FleetHub{Name: "other", Endpoint: "o:4021", Address: "10.86.0.1", PublicKey: "k"},
	}
	_, _, err := NewJoinStep().Check(context.Background(), env)
	if err == nil {
		t.Fatal("a member of one hub was moved to another")
	}
	if !strings.Contains(err.Error(), "other") || !strings.Contains(err.Error(), "machine remove") {
		t.Errorf("error %q, want it to name the current hub and the way out", err)
	}
}

func TestJoinStepRefusesADamagedTicket(t *testing.T) {
	env, _ := testEnv(t, testutil.New())
	env.Opts.Join = JoinSpec{Token: "not-a-token"}
	if _, _, err := NewJoinStep().Check(context.Background(), env); err == nil {
		t.Fatal("Check accepted a token that is not one")
	}
}

func TestJoinSpecRedacts(t *testing.T) {
	_, token := aTicket(t)
	r := JoinSpec{Token: token, Name: "m1"}.Redacted()
	if strings.Contains(r.Token, "caramelo-join") || r.Token == token {
		t.Errorf("the token survived redaction: %q", r.Token)
	}
	if r.Name != "m1" {
		t.Errorf("redaction took more than the token: %+v", r)
	}
}

func TestEdgeSocketUnitOnAPrivateMember(t *testing.T) {
	cfg := serverconfig.Default()
	cfg.Edge, cfg.HTTP3 = true, true

	public := EdgeSocketUnitContent(cfg)
	for _, want := range []string{"ListenStream=80", "ListenStream=443", "ListenDatagram=443"} {
		if !strings.Contains(public, want) {
			t.Errorf("a hub's socket unit does not carry %q:\n%s", want, public)
		}
	}
	if strings.Contains(public, "127.0.0.1") {
		t.Errorf("a hub's socket unit binds loopback:\n%s", public)
	}

	cfg.Fleet.Private = true
	private := EdgeSocketUnitContent(cfg)
	for _, want := range []string{
		"ListenStream=127.0.0.1:80", "ListenStream=127.0.0.1:443", "ListenDatagram=127.0.0.1:443",
	} {
		if !strings.Contains(private, want) {
			t.Errorf("a private member's socket unit does not carry %q:\n%s", want, private)
		}
	}
	for _, unwanted := range []string{"ListenStream=80", "ListenStream=443", "ListenDatagram=443"} {
		if strings.Contains(private, "\n"+unwanted) {
			t.Errorf("a private member's socket unit still binds %q publicly:\n%s", unwanted, private)
		}
	}
	if !strings.Contains(private, "served through its hub") {
		t.Errorf("a private member's socket unit does not say why it is loopback-only:\n%s", private)
	}
}
