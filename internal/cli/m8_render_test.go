package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/machine"
	"github.com/plytz/caramelo/internal/progress"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func fleetNow() time.Time { return time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC) }

func fleetFixture() []fleet.Machine {
	now := fleetNow()
	return []fleet.Machine{{
		Name: "hub", Role: fleet.RoleHub, Arch: "amd64", OS: "linux",
		Subnet: netip.MustParsePrefix("10.86.0.0/16"), Envs: 3,
		PublicKey: "aGVsbG8gaHViIGtleQ==", JoinedAt: now.Add(-72 * time.Hour),
		Gauge: &machine.Record{Memory: machine.Memory{
			TotalBytes: 1610612736, AvailableBytes: 1181116006}},
	}, {
		Name: "nx2", Role: fleet.RoleMember, Arch: "arm64", OS: "linux",
		Subnet: netip.MustParsePrefix("10.87.0.0/16"), Envs: 2, Private: true,
		Endpoint: "192.168.0.74:41820", PublicKey: "aGVsbG8gcGkyIGtleQ==",
		JoinedAt: now.Add(-24 * time.Hour), LastSeen: now.Add(-4 * time.Second),
		Gauge: &machine.Record{Memory: machine.Memory{
			TotalBytes: 949293056, AvailableBytes: 566231040}},
	}, {
		Name: "nx3", Role: fleet.RoleMember, Arch: "arm64", OS: "linux",
		Subnet:   netip.MustParsePrefix("10.88.0.0/16"),
		JoinedAt: now.Add(-2 * time.Hour), LastSeen: now.Add(-5 * time.Minute),
	}}
}

func quietFleetFixture() []fleet.Machine {
	ms := fleetFixture()
	for i := range ms {
		ms[i].LastSeen = time.Time{}
	}
	return ms
}

func machineDetailFixture() *api.MachineDetail {
	m := quietFleetFixture()[1]
	return &api.MachineDetail{
		Machine: m,
		Gauge: &machine.Record{
			CPU:    machine.CPU{Count: 4, Model: "ARM Cortex-A53"},
			Memory: machine.Memory{TotalBytes: 949293056, AvailableBytes: 566231040},
			OS: machine.OS{ID: "debian", VersionID: "13", Codename: "trixie",
				Kernel: "6.18.34+rpt-rpi-v8", Arch: "aarch64"},
		},
		Envs: []fleet.DirectoryEntry{
			{App: "shop", Env: "feat-x", Machine: "nx2", Address: "10.87.1.7",
				Owner: "agent-a", Mode: "dev", Via: "hub"},
			{App: "shop", Env: "staging", Machine: "nx2", Address: "10.87.1.8",
				Owner: "ci", Mode: "release", Via: "hub"},
		},
	}
}

func machineTokenFixture() *api.MachineTokenResult {
	return &api.MachineTokenResult{
		Token:     "0f1e2d3c4b5a69788796a5b4c3d2e1f0",
		Hub:       "hub",
		Endpoint:  "hub.example.com:4021",
		PublicKey: "aGVsbG8gaHViIGtleQ==",
		ExpiresAt: fleetNow().Add(time.Hour),
	}
}

func machineAddedFixture() *api.MachineAddResult {
	return &api.MachineAddResult{Machine: quietFleetFixture()[1], Joined: true}
}

func machineJoinedFixture() *api.MachineJoinResult {
	ms := quietFleetFixture()
	return &api.MachineJoinResult{Machine: ms[1], Hub: fleet.Machine{
		Name: "hub", Role: fleet.RoleHub, Endpoint: "hub.example.com:4021",
		Subnet: netip.MustParsePrefix("10.86.0.0/16"),
	}, Changed: true}
}

func fleetEnvRows() []envRow {
	rows := envRows([]env.Env{sampleEnv(), releaseEnvFixture()})
	rows[0].Machine, rows[0].Owner, rows[0].Via = "nx2", "agent-a", "hub"
	rows[1].Machine, rows[1].Owner, rows[1].Via = "hub", "ci", "node"
	return rows
}

func viaEdgeStatus() *edge.Status {
	st := steadyEdgeStatus()
	st.Routes = append(st.Routes, edge.Route{
		Host: "feat-p.shop.test", Kind: edge.KindVia, App: "shop", Env: "feat-p",
		Service: "web", Via: "nx2",
		Targets: []edge.Target{{Replica: 1, Port: 40001, State: edge.TargetActive, Inflight: 4}},
	})
	st.Ingress = &edge.IngressStatus{Enabled: true, Addr: "127.0.0.1:8443", Requests: 128, Inflight: 4}
	return st
}

type fleetService struct {
	api.Service

	machines []fleet.Machine
	detail   *api.MachineDetail
	token    *api.MachineTokenResult
	envs     []env.Env
	handed   *env.Env
	exposed  *api.ExposeResult
	err      error

	redeemed   api.RedeemRequest
	infoName   string
	removeReq  api.MachineRemoveRequest
	handoffReq api.HandoffRequest
	exposeReq  api.ExposeRequest
}

func (s *fleetService) Machines(context.Context) ([]fleet.Machine, error) {
	return s.machines, s.err
}

func (s *fleetService) MachineInfo(_ context.Context, name string) (*api.MachineDetail, error) {
	s.infoName = name
	return s.detail, s.err
}

func (s *fleetService) MachineToken(context.Context, api.MachineTokenRequest) (*api.MachineTokenResult, error) {
	return s.token, s.err
}

func (s *fleetService) MachineRemove(_ context.Context, req api.MachineRemoveRequest, _ io.Writer) error {
	s.removeReq = req
	return s.err
}

func (s *fleetService) Envs(context.Context, string) ([]env.Env, error) { return s.envs, s.err }

func (s *fleetService) EnvHandoff(_ context.Context, req api.HandoffRequest) (*env.Env, error) {
	s.handoffReq = req
	return s.handed, s.err
}

func (s *fleetService) EnvExpose(_ context.Context, req api.ExposeRequest) (*api.ExposeResult, error) {
	s.exposeReq = req
	return s.exposed, s.err
}

func TestFleetCommandsAskTheDaemon(t *testing.T) {
	svc := &fleetService{detail: machineDetailFixture()}
	if code, _, stderr := runWithService(t, svc, "machine", "show", "nx2"); code != ExitOK {
		t.Fatalf("machine show nx2: exit %d (%s)", code, stderr)
	}
	if svc.infoName != "nx2" {
		t.Errorf("machine show asked about %q", svc.infoName)
	}

	svc = &fleetService{}
	if code, _, stderr := runWithService(t, svc, "machine", "remove", "nx3", "--force", "--yes"); code != ExitOK {
		t.Fatalf("machine remove: exit %d (%s)", code, stderr)
	}
	if svc.removeReq != (api.MachineRemoveRequest{Name: "nx3", Force: true}) {
		t.Errorf("machine remove asked %+v", svc.removeReq)
	}

	e := sampleEnv()
	svc = &fleetService{handed: &e}
	code, out, stderr := runWithService(t, svc,
		"env", "handoff", "feat-x", "--app", "shop", "--to", "agent-b")
	if code != ExitOK {
		t.Fatalf("env handoff: exit %d (%s)", code, stderr)
	}
	if svc.handoffReq != (api.HandoffRequest{App: "shop", Name: "feat-x", To: "agent-b"}) {
		t.Errorf("env handoff asked %+v", svc.handoffReq)
	}
	if !strings.Contains(out, "shop/feat-x") {
		t.Errorf("handoff printed %q", out)
	}
}

func TestMachineShowNameJSONIsIndented(t *testing.T) {
	svc := &fleetService{detail: machineDetailFixture()}
	code, out, stderr := runWithService(t, svc, "machine", "show", "nx2", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d (%s)", code, stderr)
	}
	if !strings.Contains(out, "\n  \"machine\": {") {
		t.Errorf("machine show NAME --json is not indented:\n%s", out)
	}
	var back api.MachineDetail
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Machine.Name != "nx2" || len(back.Envs) != 2 {
		t.Errorf("the answer lost something: %+v", back)
	}
}

func TestMachineRemoveNeedsYes(t *testing.T) {
	svc := &fleetService{}
	code, _, stderr := runWithService(t, svc, "machine", "remove", "nx3")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d (usage)", code, ExitUsage)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("stderr = %q", stderr)
	}
	if svc.removeReq.Name != "" {
		t.Errorf("the daemon was asked anyway: %+v", svc.removeReq)
	}

	var stdout, errBuf bytes.Buffer
	if code := Run([]string{"machine", "remove", "nx3"}, &stdout, &errBuf); code != ExitUsage {
		t.Errorf("on a client: exit = %d, want %d (usage)", code, ExitUsage)
	}
	if !strings.Contains(errBuf.String(), "--yes") {
		t.Errorf("client stderr = %q", errBuf.String())
	}
}

func TestExposeViaTakesTheFleetPath(t *testing.T) {
	svc := &fleetService{exposed: exposeFixture()}
	if code, _, stderr := runWithService(t, svc,
		"env", "expose", "feat-x", "--app", "shop", "--via", "hub"); code != ExitOK {
		t.Fatalf("env expose --via: exit %d (%s)", code, stderr)
	}
	if svc.exposeReq.Via != config.ViaHub || svc.exposeReq.Name != "feat-x" {
		t.Errorf("env expose --via asked %+v", svc.exposeReq)
	}

	edgeSvc := &edgeService{expose: exposeFixture()}
	if code, _, stderr := runWithService(t, edgeSvc, "env", "expose", "feat-x", "--app", "shop"); code != ExitOK {
		t.Fatalf("env expose: exit %d (%s)", code, stderr)
	}
	if edgeSvc.exposeReq.Name != "feat-x" {
		t.Errorf("env expose asked %+v", edgeSvc.exposeReq)
	}
}

func TestEnvListMine(t *testing.T) {
	svc := &fleetService{envs: []env.Env{sampleEnv()}}
	code, _, stderr := runWithService(t, svc, "env", "list", "--mine")
	if code != ExitUsage {
		t.Errorf("on the socket: exit = %d, want %d (usage)", code, ExitUsage)
	}
	if !strings.Contains(stderr, "--mine needs an identity") {
		t.Errorf("stderr = %q", stderr)
	}

	var stdout, errBuf bytes.Buffer
	code = RunWith(context.Background(), []string{"env", "list", "--mine"}, &stdout, &errBuf, Options{
		Service: svc,
		Session: api.Session{Transport: "ssh", Identity: "agent-a", Machine: "hub"},
	})
	if code != ExitOK {
		t.Fatalf("exit = %d (%s)", code, errBuf.String())
	}
	if !strings.Contains(stdout.String(), "no environments") {
		t.Errorf("an environment with no owner was counted as agent-a's:\n%s", stdout.String())
	}
}

func TestEnvListColumnsAppearWithTheFleet(t *testing.T) {
	alone := envsTable(envRows([]env.Env{sampleEnv()}))
	head := alone.Rows()
	if len(head) != 1 || len(head[0]) != 10 {
		t.Errorf("a machine of one printed %d cells, want 10", len(head[0]))
	}
	with := envsTable(fleetEnvRows())
	if rows := with.Rows(); len(rows[0]) != 12 {
		t.Errorf("a hub printed %d cells, want 12", len(rows[0]))
	}
	out := ui.NewView().Table(with).String()
	for _, want := range []string{"MACHINE", "OWNER", "nx2", "agent-a"} {
		if !strings.Contains(out, want) {
			t.Errorf("the table does not say %q:\n%s", want, out)
		}
	}
	if plain := ui.NewView().Table(alone).String(); strings.Contains(plain, "MACHINE") {
		t.Errorf("a machine of one grew a MACHINE column:\n%s", plain)
	}
}

func TestRouteTablesGrowViaOnlyWhenThereIsOne(t *testing.T) {
	own := []edge.Route{{Host: "feat-x.shop.test", Kind: edge.KindHTTPS, Service: "web"}}
	if out := ui.NewView().Table(routesTable(own)).String(); strings.Contains(out, "VIA") {
		t.Errorf("a route served here grew a VIA column:\n%s", out)
	}
	elsewhere := append(own, edge.Route{
		Host: "feat-p.shop.test", Kind: edge.KindVia, Service: "web", Via: "nx2"})
	out := ui.NewView().Table(routesTable(elsewhere)).String()
	for _, want := range []string{"VIA", "nx2"} {
		if !strings.Contains(out, want) {
			t.Errorf("the route table does not say %q:\n%s", want, out)
		}
	}
}

func TestEdgeStatusSaysViaAndNotReplica(t *testing.T) {
	out := ui.NewView().Table(edgeRoutesTable(viaEdgeStatus().Routes)).String()
	if !strings.Contains(out, "nx2") {
		t.Errorf("edge status does not name the machine serving the name:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "feat-p.shop.test") && !strings.Contains(line, " -") {
			t.Errorf("a via route printed a replica number: %q", line)
		}
	}
}

func TestEdgeStatusIngressLine(t *testing.T) {
	if line := describeIngress(nil); line != "" {
		t.Errorf("a machine with no ingress printed %q", line)
	}
	line := describeIngress(&edge.IngressStatus{Enabled: true, Addr: "127.0.0.1:8443", Requests: 128, Inflight: 4})
	for _, want := range []string{"127.0.0.1:8443", "128 request(s)", "4 in flight"} {
		if !strings.Contains(line, want) {
			t.Errorf("the ingress line does not say %q: %q", want, line)
		}
	}
	broken := describeIngress(&edge.IngressStatus{Enabled: true, Error: "address in use"})
	if !strings.Contains(broken, "address in use") {
		t.Errorf("a listener that is not open does not say why: %q", broken)
	}
}

func TestDescribeSeen(t *testing.T) {
	now := fleetNow()
	ms := fleetFixture()
	for _, tc := range []struct {
		m    fleet.Machine
		want string
	}{
		{ms[0], "-"},
		{ms[1], "4s ago"},
		{ms[2], "unreachable (5m0s ago)"},
		{fleet.Machine{Name: "nx4", Role: fleet.RoleMember}, "never"},

		{fleet.Machine{Role: fleet.RoleMember, LastSeen: now.Add(-fleet.UnreachableAfter)}, "1m40s ago"},
	} {
		if got := describeSeen(tc.m, now); got != tc.want {
			t.Errorf("%s: seen = %q, want %q", tc.m.Name, got, tc.want)
		}
	}
}

func TestDescribeFleet(t *testing.T) {
	now := fleetNow()
	if got := describeFleet(nil, "hub", now); got != "a fleet of one" {
		t.Errorf("no fleet = %q", got)
	}
	if got := describeFleet(fleetFixture()[:1], "hub", now); got != "a fleet of one" {
		t.Errorf("one machine = %q", got)
	}
	got := describeFleet(fleetFixture(), "hub", now)
	for _, want := range []string{"hub of 3 machines", "2 member(s)", "1 unreachable"} {
		if !strings.Contains(got, want) {
			t.Errorf("the fleet line does not say %q: %q", want, got)
		}
	}
	if got := describeFleet(fleetFixture(), "nx2", now); !strings.HasPrefix(got, "member of") {
		t.Errorf("a member's own status calls it %q", got)
	}
}

func TestStatusToleratesADaemonWithNoFleet(t *testing.T) {
	if ms := fleetOf(context.Background(), &fakeAPI{machinesErr: api.ErrNotImplemented}); ms != nil {
		t.Errorf("a daemon that predates M8 reported a fleet: %+v", ms)
	}
	ms := fleetOf(context.Background(), &fakeAPI{machinesErr: errors.New("the store is locked")})
	if len(ms) != 1 || !strings.Contains(ms[0].Name, "the store is locked") {
		t.Errorf("a broken fleet lookup was swallowed: %+v", ms)
	}
}

func TestFeedLineNamesTheMachine(t *testing.T) {
	e := progress.Event{
		At: time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC), App: "shop", Env: "feat-x",
		Action: "up", Step: "start", Status: "ok", Detail: "web started",
	}
	own := ui.FeedLine(e)
	if strings.Contains(own, "nx2") {
		t.Errorf("an event with no machine named one: %q", own)
	}
	e.Machine = "nx2"
	withMachine := ui.FeedLine(e)
	if !strings.Contains(withMachine, "nx2") {
		t.Errorf("the feed does not say where it happened: %q", withMachine)
	}
	if !strings.Contains(withMachine, "shop/feat-x") {
		t.Errorf("the machine displaced the environment: %q", withMachine)
	}
}

func TestMachineAddRefusesAnEdgeOnAPrivateMachine(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"machine", "add", "admin@nx2.local", "--edge", "--private"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d (usage)", code, ExitUsage)
	}
	if !strings.Contains(stderr.String(), "opposites") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestMachineJoinNeedsAToken(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run([]string{"machine", "join", "hub.example.com"}, &stdout, &stderr)
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d (usage)", code, ExitUsage)
	}
	if !strings.Contains(stderr.String(), "--token") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestEnvListAllWalksTheDirectory(t *testing.T) {
	svc := &envService{
		envs: []env.Env{{App: "shop", Name: "feat-x", Status: env.StatusReady, Mode: env.ModeDev}},
		directory: []fleet.DirectoryEntry{
			{App: "shop", Env: "feat-x", Machine: "hub"},
			{App: "shop", Env: "feat-y", Machine: "m1", Owner: "agent-b", Address: "10.87.1.4"},
		},
	}
	var stdout, errBuf bytes.Buffer
	code := RunWith(context.Background(), []string{"env", "list", "--all", "--app", "shop"},
		&stdout, &errBuf, Options{Service: svc, Session: api.Session{Transport: "socket"}})
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errBuf.String())
	}
	out := stdout.String()
	for _, want := range []string{"feat-x", "feat-y", "m1", "agent-b", "MACHINE", "OWNER"} {
		if !strings.Contains(out, want) {
			t.Errorf("env list --all does not mention %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "feat-x"); n != 1 {
		t.Errorf("feat-x appears %d times; a row the directory and the machine both hold is one row:\n%s", n, out)
	}
}

func TestAMachineOfOneIsARowButNotAFleetLine(t *testing.T) {
	now := fleetNow()
	svc := &fleetService{machines: fleetFixture()[:1]}
	var out, errBuf bytes.Buffer
	if code := RunWith(context.Background(), []string{"machine", "list"}, &out, &errBuf,
		Options{Service: svc, Session: api.Session{Transport: "socket"}}); code != ExitOK {
		t.Fatalf("machine list: exit %d (%s)", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "hub") {
		t.Errorf("machine list on a machine of one says nothing about it:\n%s", out.String())
	}

	st := &api.Status{Hostname: "hub", Machine: "hub", Version: "test"}
	one := plainOf(t, statusView(st, fleetFixture()[:1], now))
	if strings.Contains(one, "fleet") {
		t.Errorf("status on a machine of one prints a fleet row:\n%s", one)
	}
	two := plainOf(t, statusView(st, fleetFixture(), now))
	if !strings.Contains(two, "fleet") {
		t.Errorf("status on a hub with a member says nothing about the fleet:\n%s", two)
	}
}

func plainOf(t *testing.T, v *ui.View) string {
	t.Helper()
	var b bytes.Buffer
	if err := v.Write(&b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

func TestMachineAddPointsTheTicketAtTheHubItIsTalkingTo(t *testing.T) {
	ticket := fleet.Ticket{
		Hub: "hub", Endpoint: "10.0.2.15:4021", PublicKey: "k", Address: "10.86.0.1",
		Peer: "10.86.0.9", Range: fleet.FleetRange, Secret: "s3cret",
	}
	blob, err := ticket.Encode()
	if err != nil {
		t.Fatal(err)
	}
	a := &app{machine: "caramelo@192.168.56.11:4022"}
	out := a.hubReachableFrom(blob)
	back, err := fleet.ParseTicket(out)
	if err != nil {
		t.Fatalf("the rewritten ticket does not parse: %v", err)
	}
	if back.Endpoint != "192.168.56.11:4021" {
		t.Errorf("endpoint = %q, want the address this computer uses with the hub's port", back.Endpoint)
	}
	if back.Secret != ticket.Secret || back.PublicKey != ticket.PublicKey {
		t.Errorf("the rewrite changed more than the endpoint: %+v", back.Redacted())
	}
	if got := a.hubReachableFrom("not a ticket"); got != "not a ticket" {
		t.Errorf("a blob that is not a ticket was rewritten to %q", got)
	}
}

func TestTheMachineToMachineVerbsReadTheSessionsStdin(t *testing.T) {
	svc := &fleetService{}
	body := `{"secret":"s3cret","name":"m1","public_key":"k","arch":"amd64"}`
	var stdout, stderr bytes.Buffer
	code := RunWith(withStdin(context.Background(), strings.NewReader(body)),
		[]string{"machine", "redeem"}, &stdout, &stderr,
		Options{Service: svc, Session: api.Session{Transport: "tunnel", Identity: "m1", Peer: "m1"}})
	if code != ExitOK {
		t.Fatalf("machine redeem: exit %d (%s)", code, stderr.String())
	}
	if svc.redeemed.Secret != "s3cret" || svc.redeemed.Name != "m1" {
		t.Errorf("the daemon was given %+v, want the document on the session's stdin", svc.redeemed)
	}
}

func (s *fleetService) MachineRedeem(_ context.Context, req api.RedeemRequest) (*api.RedeemResult, error) {
	s.redeemed = req
	return &api.RedeemResult{
		Machine: fleet.Machine{Name: req.Name, Role: fleet.RoleMember},
		Hub:     fleet.Machine{Name: "hub", Role: fleet.RoleHub},
		Changed: true,
	}, nil
}
