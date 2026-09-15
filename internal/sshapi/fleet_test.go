package sshapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/state"
)

type fleetState struct {
	rows map[int64]env.FleetRow
	dir  map[[2]string]fleet.DirectoryEntry
}

func newFleetState() *fleetState {
	return &fleetState{rows: map[int64]env.FleetRow{}, dir: map[[2]string]fleet.DirectoryEntry{}}
}

func (f *fleetState) EnvFleetRow(_ context.Context, id int64) (env.FleetRow, error) {
	return f.rows[id], nil
}

func (f *fleetState) EnvFleetRows(context.Context, string) (map[int64]env.FleetRow, error) {
	out := make(map[int64]env.FleetRow, len(f.rows))
	for k, v := range f.rows {
		out[k] = v
	}
	return out, nil
}

func (f *fleetState) SetEnvFleetRow(_ context.Context, id int64, row env.FleetRow) error {
	f.rows[id] = row
	return nil
}

func (f *fleetState) PutDirectoryEntry(_ context.Context, e fleet.DirectoryEntry) error {
	f.dir[[2]string{e.App, e.Env}] = e
	return nil
}

func (f *fleetState) RemoveDirectoryEntry(_ context.Context, app, name string) error {
	delete(f.dir, [2]string{app, name})
	return nil
}

func (f *fleetState) DirectoryEntries(_ context.Context, machine string) ([]fleet.DirectoryEntry, error) {
	out := make([]fleet.DirectoryEntry, 0, len(f.dir))
	for _, e := range f.dir {
		if machine == "" || e.Machine == machine {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fleetState) Machines(context.Context) ([]fleet.Machine, error) {
	return []fleet.Machine{{Name: "hub", Role: fleet.RoleHub}}, nil
}

func (s *envStore) add(t *testing.T, app, name string) {
	t.Helper()
	if _, err := s.CreateEnv(context.Background(), state.EnvRecord{
		App: app, Name: name, Branch: name, Status: state.EnvReady, Mode: state.EnvModeDev,

		PortBase: 20000 + len(s.envs)*32, PortCount: 32,
	}); err != nil {
		t.Fatalf("record env %q: %v", name, err)
	}
}

type fakeResolver struct {
	env     map[string]api.Location
	machine map[string]api.Location
	err     error
}

func (r *fakeResolver) ResolveEnv(_ context.Context, app, name string) (api.Location, error) {
	if r.err != nil {
		return api.Location{}, r.err
	}
	if loc, ok := r.env[app+"/"+name]; ok {
		return loc, nil
	}
	return api.Location{Local: true, Reachable: true}, nil
}

func (r *fakeResolver) ResolveMachine(_ context.Context, name string) (api.Location, error) {
	if r.err != nil {
		return api.Location{}, r.err
	}
	if loc, ok := r.machine[name]; ok {
		return loc, nil
	}
	return api.Location{Local: true, Reachable: true}, nil
}

type fakeForwarder struct {
	argv []string
	loc  api.Location
	code int
	err  error

	out string

	identity string
}

func (f *fakeForwarder) Forward(ctx context.Context, loc api.Location, argv []string,
	_ io.Reader, stdout, _ io.Writer) (int, error) {
	f.argv, f.loc = argv, loc
	f.identity = env.IdentityFrom(ctx)
	if f.err != nil {
		return 1, f.err
	}
	if f.out != "" && stdout != nil {
		if _, err := io.WriteString(stdout, f.out); err != nil {
			return 1, err
		}
	}
	return f.code, nil
}

func fleetSession(t *testing.T, argv ...string) (context.Context, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	return WithSession(context.Background(), api.Session{
		Transport: "tunnel", Identity: "commander", Machine: "hub",
		Args: argv, Stdout: &out, Stderr: io.Discard,
	}), &out
}

func remote(machine string) api.Location {
	return api.Location{Machine: machine, Address: "10.87.0.1:4022", Reachable: true}
}

func TestACommandForAnEnvironmentOnAMemberIsForwardedWithItsOwnArgv(t *testing.T) {
	fwd := &fakeForwarder{code: 3, out: `{"id":1,"name":"feat-x"}` + "\n"}
	d := &Daemon{
		EnvManager: &env.Manager{},
		Resolver:   &fakeResolver{env: map[string]api.Location{"shop/feat-x": remote("nx2")}},
		Forwarder:  fwd,
	}
	ctx, out := fleetSession(t, "up", "feat-x", "--app", "shop", "--json", "--progress", "json")

	_, err := d.Up(ctx, env.UpRequest{App: "shop", Name: "feat-x"}, io.Discard)
	var fe *api.ForwardedError
	if !errors.As(err, &fe) {
		t.Fatalf("error = %v, want an api.ForwardedError", err)
	}
	if fe.Code != 3 || fe.Machine != "nx2" {
		t.Fatalf("forwarded = %+v, want nx2 exit 3", fe)
	}
	if strings.Join(fwd.argv, " ") != "up feat-x --app shop --json --progress json" {
		t.Fatalf("argv = %v, want the session's own", fwd.argv)
	}
	if out.String() != `{"id":1,"name":"feat-x"}`+"\n" {
		t.Fatalf("stdout = %q, want the member's own bytes", out.String())
	}
}

func TestAnEnvironmentOnThisMachineIsNotForwardedAtAll(t *testing.T) {
	fwd := &fakeForwarder{}
	d := &Daemon{Resolver: &fakeResolver{}, Forwarder: fwd}
	ctx, _ := fleetSession(t, "env", "show", "feat-x")

	if err := d.forwardEnv(ctx, "shop", "feat-x"); err != nil {
		t.Fatalf("forwardEnv: %v", err)
	}
	if fwd.argv != nil {
		t.Fatalf("a local environment was forwarded: %v", fwd.argv)
	}
}

func TestAMachineOfOneNeverForwards(t *testing.T) {
	d := &Daemon{}
	ctx, _ := fleetSession(t, "up", "feat-x")
	if err := d.forwardEnv(ctx, "shop", "feat-x"); err != nil {
		t.Fatalf("forwardEnv on a machine of one: %v", err)
	}
}

func TestAForwardThatCouldNotBeMadeIsAnOrdinaryError(t *testing.T) {
	fwd := &fakeForwarder{err: errors.New("machine \"nx2\" is unreachable")}
	d := &Daemon{
		Resolver:  &fakeResolver{env: map[string]api.Location{"shop/feat-x": remote("nx2")}},
		Forwarder: fwd,
	}
	ctx, _ := fleetSession(t, "up", "feat-x")
	err := d.forwardEnv(ctx, "shop", "feat-x")
	var fe *api.ForwardedError
	if err == nil || errors.As(err, &fe) {
		t.Fatalf("error = %v, want an ordinary failure and not a forwarded exit code", err)
	}
}

func TestAnInProcessCallCannotBeForwarded(t *testing.T) {
	d := &Daemon{
		Resolver:  &fakeResolver{env: map[string]api.Location{"shop/feat-x": remote("nx2")}},
		Forwarder: &fakeForwarder{},
	}
	err := d.forwardEnv(context.Background(), "shop", "feat-x")
	if err == nil || !strings.Contains(err.Error(), "no invocation to forward") {
		t.Fatalf("error = %v, want one about there being no invocation", err)
	}
}

func TestAMachinesSessionIsNeverForwardedOnwards(t *testing.T) {
	d := &Daemon{
		Resolver:  &fakeResolver{env: map[string]api.Location{"shop/feat-x": remote("nx3")}},
		Forwarder: &fakeForwarder{},
	}
	ctx := WithSession(context.Background(), api.Session{
		Transport: "tunnel", Identity: "nx2", Peer: "nx2",
		Args: []string{"up", "feat-x"}, Stdout: io.Discard,
	})
	err := d.forwardEnv(ctx, "shop", "feat-x")
	if err == nil || !strings.Contains(err.Error(), "not forwarded twice") {
		t.Fatalf("error = %v, want a refusal to forward a machine's own session", err)
	}
}

func machines(names ...string) MachineLookup {
	return MachineLookupFunc(func(_ context.Context, name string) (fleet.Machine, bool) {
		for _, n := range names {
			if n == name {
				return fleet.Machine{Name: n, Role: fleet.RoleMember, LastSeen: time.Now()}, true
			}
		}
		return fleet.Machine{}, false
	})
}

func TestAnAnnouncementFromACommanderIsRefusedNamingWhatToRunInstead(t *testing.T) {
	d := &Daemon{KnownMachines: machines("nx2")}
	ctx := WithSession(context.Background(), api.Session{Transport: "tunnel", Identity: "commander"})

	_, err := d.MachineAnnounce(ctx, fleet.Announcement{Machine: "nx2"})
	if err == nil || !strings.Contains(err.Error(), "machine list") {
		t.Fatalf("error = %v, want a refusal naming the command a person runs", err)
	}
	if !strings.Contains(err.Error(), "commander") {
		t.Fatalf("error = %v, want the peer named", err)
	}
}

func TestAServerNamesAMachinePeerAndNobodyElse(t *testing.T) {
	s := &Server{Machines: machines("nx2")}
	if got := s.machinePeer(context.Background(), "nx2"); got != "nx2" {
		t.Fatalf("machinePeer(nx2) = %q, want nx2", got)
	}
	if got := s.machinePeer(context.Background(), "commander"); got != "" {
		t.Fatalf("machinePeer(commander) = %q, want a commander to be nobody's machine", got)
	}

	if got := (&Server{}).machinePeer(context.Background(), "nx2"); got != "" {
		t.Fatalf("machinePeer on a machine of one = %q, want empty", got)
	}
}

func TestAnAnnouncementIsWrittenAgainstTheMachineThatSentIt(t *testing.T) {
	st := newFleetState()
	m := (&env.Manager{}).WithFleet(env.FleetWiring{Fleet: st, Machine: "hub"})
	d := &Daemon{EnvManager: m, KnownMachines: machines("nx2"), Now: func() time.Time { return time.Unix(1, 0) }}
	ctx := WithSession(context.Background(), api.Session{Transport: "tunnel", Identity: "nx2", Peer: "nx2"})

	res, err := d.MachineAnnounce(ctx, fleet.Announcement{

		Machine: "hub", Full: true,
		Envs: []fleet.DirectoryEntry{{App: "shop", Env: "feat-x", Machine: "hub"}},
	})
	if err != nil {
		t.Fatalf("MachineAnnounce: %v", err)
	}
	if res.Accepted != 1 {
		t.Fatalf("accepted = %d, want 1", res.Accepted)
	}
	entries, err := st.DirectoryEntries(ctx, "")
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	if len(entries) != 1 || entries[0].Machine != "nx2" {
		t.Fatalf("directory = %+v, want the row against nx2", entries)
	}
}

func TestEnvsMineFiltersOnTheCallingPeer(t *testing.T) {
	st := newFleetState()
	st.rows[1] = env.FleetRow{Owner: "agent-a"}
	st.rows[2] = env.FleetRow{Owner: "agent-b"}
	store := newEnvStore()
	m := (&env.Manager{Store: store}).WithFleet(env.FleetWiring{Fleet: st, Machine: "hub"})
	d := &Daemon{EnvManager: m}
	store.add(t, "shop", "feat-x")
	store.add(t, "shop", "feat-y")

	ctx := WithSession(context.Background(), api.Session{Identity: "agent-a"})
	mine, err := d.EnvsMine(ctx, "shop")
	if err != nil {
		t.Fatalf("EnvsMine: %v", err)
	}
	if len(mine) != 1 || mine[0].Name != "feat-x" {
		t.Fatalf("mine = %+v, want feat-x only", mine)
	}
}

func TestAForwardedCommandSaysWhoseWorkItIs(t *testing.T) {
	fwd := &fakeForwarder{}
	d := &Daemon{
		Resolver:  &fakeResolver{env: map[string]api.Location{"shop/feat-x": remote("nx2")}},
		Forwarder: fwd,
	}
	ctx, _ := fleetSession(t, "env", "show", "feat-x", "--app", "shop")

	if err := d.forwardEnv(ctx, "shop", "feat-x"); err == nil {
		t.Fatal("forwardEnv answered locally")
	}
	if fwd.identity != "commander" {
		t.Fatalf("the forward is for %q, want the commander that asked", fwd.identity)
	}
}

func TestOnlyAMachineOfTheFleetMaySayWhoACommandIsFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		sess api.Session
		want string
	}{
		{"a hub speaking for a commander", api.Session{Identity: "hub", Peer: "hub", OnBehalfOf: "commander"}, "commander"},
		{"a commander claiming to be another", api.Session{Identity: "commander-a", OnBehalfOf: "commander-b"}, "commander-a"},
		{"a machine that said nothing", api.Session{Identity: "hub", Peer: "hub"}, "hub"},
		{"an ordinary session", api.Session{Identity: "commander-a"}, "commander-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sess.Author(); got != tc.want {
				t.Fatalf("Author() = %q, want %q", got, tc.want)
			}
			d := &Daemon{}
			if got := env.IdentityFrom(d.withIdentity(WithSession(context.Background(), tc.sess))); got != tc.want {
				t.Fatalf("the identity on the context = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOneVariableOutOfASessionsEnvironment(t *testing.T) {
	environ := []string{"LANG=C", api.IdentityEnv + "=commander-b", "TERM=xterm"}
	if got := envValue(environ, api.IdentityEnv); got != "commander-b" {
		t.Fatalf("envValue = %q, want %q", got, "commander-b")
	}
	if got := envValue(environ, "NOTHING"); got != "" {
		t.Fatalf("envValue of a variable that was not sent = %q, want empty", got)
	}
}
