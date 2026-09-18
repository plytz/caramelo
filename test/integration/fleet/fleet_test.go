//go:build integration

package fleet

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	capi "github.com/plytz/caramelo/internal/api"
	cedge "github.com/plytz/caramelo/internal/edge"
	cenv "github.com/plytz/caramelo/internal/env"
	cfleet "github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/place"
	cprogress "github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/test/integration/itest"
)

func TestAMemberJoins(t *testing.T) {
	begin(t)
	box := memberBox(t, 0)
	if box == nil {
		t.Skip("fleet suite: no member was bound")
	}
	repo = initSampleRepo(t)

	buildCtx, cancelBuild := context.WithTimeout(context.Background(), itest.Scale(10*time.Minute))
	_, buildErr := itest.BinaryFor(buildCtx, box)
	cancelBuild()
	if buildErr != nil {
		t.Fatalf("build the binary `member add` ships to %s: %v", box.Alias, buildErr)
	}

	target := box.BootstrapTarget(t)
	name := memberNames[0]

	add, res := machineAdd(t, target, name, memberEdgeArgs()...)
	if res.ExitCode != 0 {
		t.Fatalf("member add %s: exit %d\nstdout:\n%sstderr:\n%s", target, res.ExitCode, res.Stdout, res.Stderr)
	}
	if !add.Joined {
		t.Error("member add of a box that was clean reports Joined=false")
	}
	if add.Setup == nil {
		t.Error("member add --json carries no setup report; the M2 bootstrap it ran is half of what it did")
	}
	if add.Machine.Name != name {
		t.Fatalf("member add named the machine %q, want %q", add.Machine.Name, name)
	}
	joinedM1 = true
	relogin(t, box)
	if err := itest.SeedImagesTo(box, fleetImages...); err != nil {
		t.Fatalf("seed the fleet images onto %s: %v", box.Alias, err)
	}
	if err := itest.EnsureCurlOn(box); err != nil {
		t.Fatalf("curl on %s: %v", box.Alias, err)
	}
	startMemberEdge(t, box)

	list := machineList(t)
	if len(list) < 2 {
		t.Fatalf("member list = %v, want the hub and %s", machineSummary(list), name)
	}
	if !list[0].Role.IsHub() {
		t.Errorf("the first row of member list is %s (%s), want the hub", list[0].Name, list[0].Role)
	}
	m1 := machineNamed(t, list, name)
	if m1.Role != cfleet.RoleMember {
		t.Errorf("%s has role %q, want %q", name, m1.Role, cfleet.RoleMember)
	}
	wantArch := box.MustArch(t)
	if m1.Arch != wantArch {
		t.Errorf("%s reports arch %q, want %q (a GOARCH, never uname -m's)", name, m1.Arch, wantArch)
	}
	if !m1.Subnet.IsValid() {
		t.Fatalf("%s holds no subnet: %+v", name, m1)
	}
	if m1.Subnet == list[0].Subnet {
		t.Errorf("%s and the hub hold the same subnet %s", name, m1.Subnet)
	}
	if got := m1.Subnet.Bits(); got != 16 {
		t.Errorf("%s holds a /%d, want a /16 out of the fleet's /12", name, got)
	}
	if !cfleet.Contains(m1.Subnet) {
		t.Errorf("%s holds %s, which is outside the fleet's range %s", name, m1.Subnet, cfleet.FleetRange)
	}
	if !m1.Reachable(time.Now()) {
		t.Errorf("%s is not reachable right after joining: last seen %v", name, m1.LastSeen)
	}
	if m1.Gauge == nil {
		t.Errorf("%s announced no gauge; placement has nothing to subtract from", name)
	}

	joined, cancelJoined := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancelJoined()
	if role, err := itest.FleetRole(joined, box); err != nil {
		t.Fatalf("read the fleet role of %s: %v", box.Alias, err)
	} else if role != string(cfleet.RoleMember) {
		t.Errorf("/etc/caramelo/config.yaml on %s says role %q, want %q", box.Alias, role, cfleet.RoleMember)
	}

	hubFleet, err := itest.ServerFleet(joined, hub)
	if err != nil {
		t.Fatalf("read the fleet %s heads: %v", hub.Alias, err)
	}
	memberFleet, err := itest.ServerFleet(joined, box)
	if err != nil {
		t.Fatalf("read the fleet %s joined: %v", box.Alias, err)
	}
	if hubFleet == "" || memberFleet != hubFleet {
		t.Errorf("/etc/caramelo/config.yaml on %s says the fleet is %q, want the hub's %q",
			box.Alias, memberFleet, hubFleet)
	}
	if own := machineListOn(t, box); len(own) == 0 {
		t.Errorf("member list on %s answered with no machine at all", box.Alias)
	} else {
		var hubRow cfleet.Machine
		for _, m := range own {
			if m.Role.IsHub() {
				hubRow = m
			}
		}
		if hubRow.Name != list[0].Name {
			t.Errorf("%s calls its hub %q, want %q, the name the hub answers to",
				box.Alias, hubRow.Name, list[0].Name)
		}
	}

	itest.RunGoss(t, hub, itest.MustGossSpec(t, "fleet.yaml"))
	itest.RunGoss(t, box, itest.MustGossSpec(t, "member.yaml"))

	again, res := machineAdd(t, target, name, memberEdgeArgs()...)
	if res.ExitCode != 0 {
		t.Fatalf("a second member add: exit %d\nstderr:\n%s", res.ExitCode, res.Stderr)
	}
	if again.Joined {
		t.Error("a second member add of the same box reports Joined=true; it changed nothing")
	}
	if again.Machine.Subnet != m1.Subnet {
		t.Errorf("a second member add moved %s from %s to %s", name, m1.Subnet, again.Machine.Subnet)
	}
	after := machineList(t)
	if len(after) != len(list) {
		t.Errorf("member list = %v after a second add, want %v", machineSummary(after), machineSummary(list))
	}
}

func TestTheContextOnAMemberAndItsHub(t *testing.T) {
	begin(t)
	box := needM1(t)
	needRepo(t)

	hubCtx := itest.MustContextOn(t, hub, itest.CarameloBinary)
	if hubCtx.Role != place.RoleHub {
		t.Fatalf("the box that hubs this fleet says role %q, want %q: %+v", hubCtx.Role, place.RoleHub, hubCtx)
	}
	if hubCtx.Server == nil || hubCtx.Server.Fleet == "" {
		t.Fatalf("the hub names no fleet: %+v", hubCtx.Server)
	}
	if hubCtx.Server.Hub != nil {
		t.Errorf("the hub carries a hub it joined: %+v", hubCtx.Server.Hub)
	}

	cfg := itest.MustServerConfigOn(t, box)
	got := itest.MustContextOn(t, box, itest.CarameloBinary)
	name := memberNames[0]
	if got.Role != place.RoleMember {
		t.Fatalf("a box the hub added says role %q, want %q: %+v", got.Role, place.RoleMember, got)
	}
	if got.Name != name {
		t.Errorf("context calls the box %q and the hub calls it %q", got.Name, name)
	}
	if got.Problem != "" {
		t.Errorf("context on a member reports a problem: %s", got.Problem)
	}
	if got.Server == nil {
		t.Fatalf("context on a member carries no server half: %+v", got)
	}
	s := got.Server
	if s.Fleet != cfg.Member.Fleet || s.Fleet != hubCtx.Server.Fleet {
		t.Errorf("the member says it is in fleet %q, its config.yaml says %q and the hub hubs %q",
			s.Fleet, cfg.Member.Fleet, hubCtx.Server.Fleet)
	}
	if s.Hub == nil {
		t.Fatalf("context on a member says nothing about the hub it joined: %+v", s)
	}
	if s.Hub.Endpoint != cfg.Member.Hub.Endpoint {
		t.Errorf("context says the member dials %q, its config.yaml says %q", s.Hub.Endpoint, cfg.Member.Hub.Endpoint)
	}
	if want := fmt.Sprintf(":%d", itest.VPNPort); !strings.HasSuffix(s.Hub.Endpoint, want) {
		t.Errorf("the member dials %q, want the tunnel port %s", s.Hub.Endpoint, want)
	}

	var hubRow cfleet.Machine
	for _, row := range machineList(t) {
		if row.Role.IsHub() {
			hubRow = row
			break
		}
	}
	if hubRow.PublicKey == "" {
		t.Fatalf("member list holds no hub row with a key: %v", machineSummary(machineList(t)))
	}
	if s.Hub.PublicKey != hubRow.PublicKey {
		t.Errorf("the member holds hub key %q, the fleet's hub is %q", s.Hub.PublicKey, hubRow.PublicKey)
	}
	if addr := hubRow.Address(); addr.IsValid() && s.Hub.Address != addr.String() {
		t.Errorf("the member answers the hub at %q inside the tunnel, the hub holds %s", s.Hub.Address, addr)
	}
	if !s.Services.Daemon.Present {
		t.Errorf("context finds no caramelod socket at %s on a joined member", s.Services.Daemon.Path)
	}
	if got.TalksTo.Kind != place.TalksSocket {
		t.Errorf("a command typed on a member would talk to %q (%s), want the caramelod on this machine",
			got.TalksTo.Kind, got.TalksTo.Problem)
	}

	text := itest.ContextTextOn(t, box, itest.CarameloBinary)
	for _, want := range []string{name + ", " + place.RoleMember, "of fleet " + s.Fleet, "(hub " + s.Hub.Endpoint + ")"} {
		if !strings.Contains(text, want) {
			t.Errorf("caramelo context on %s never says %q:\n%s", box.Alias, want, text)
		}
	}
}

func TestAMemberJoinsItselfWithAToken(t *testing.T) {
	begin(t)
	box := memberBox(t, 1)
	if box == nil {
		t.Skip("fleet suite: this lab binds one member, so there is no box to join itself")
	}
	needRepo(t)
	name := memberNames[1]

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancel()
	endpoint, err := itest.HubEndpoint(ctx, hub)
	if err != nil {
		t.Fatalf("the hub's endpoint: %v", err)
	}

	tok, res := machineToken(t)
	if res.ExitCode != 0 {
		t.Fatalf("member token: exit %d\nstderr:\n%s", res.ExitCode, res.Stderr)
	}
	if strings.TrimSpace(tok.Token) == "" {
		t.Fatal("member token printed no token")
	}
	if tok.PublicKey == "" {
		t.Error("member token carries no hub public key; a joining machine has nothing to verify against")
	}
	if !tok.ExpiresAt.After(time.Now()) {
		t.Errorf("the token expires at %v, which is not in the future", tok.ExpiresAt)
	}

	throwaway, res := machineToken(t)
	if res.ExitCode != 0 {
		t.Fatalf("member token for the unprepared join: exit %d\nstderr:\n%s", res.ExitCode, res.Stderr)
	}
	unprepared := machineJoinOn(t, box, endpoint, throwaway.Token, name+"-unprepared")
	if unprepared.ExitCode == 0 {
		t.Fatalf("a join on %s, which never ran hub setup, succeeded:\nstdout:\n%sstderr:\n%s",
			box.Alias, unprepared.Stdout, unprepared.Stderr)
	}
	said := unprepared.Stdout + unprepared.Stderr
	if !strings.Contains(said, "hub setup") {
		t.Errorf("the refusal on an unprepared machine does not name hub setup:\n%s", said)
	}
	if strings.Contains(said, "no such file") {
		t.Errorf("the refusal on an unprepared machine names a missing file instead of the step that makes it:\n%s", said)
	}

	if err := itest.SetupMemberForJoin(box, true); err != nil {
		t.Fatalf("set %s up before it joins: %v", box.Alias, err)
	}

	short, res := machineToken(t, "--ttl", "1s")
	if res.ExitCode != 0 {
		t.Fatalf("member token --ttl 1s: exit %d\nstderr:\n%s", res.ExitCode, res.Stderr)
	}
	time.Sleep(2 * time.Second)
	expired := machineJoinOn(t, box, endpoint, short.Token, name+"-expired")
	if expired.ExitCode == 0 {
		t.Error("a join with an expired token succeeded")
	}
	if !strings.Contains(strings.ToLower(expired.Stdout+expired.Stderr), "expired") {
		t.Errorf("the refusal does not say the token expired:\n%s%s", expired.Stdout, expired.Stderr)
	}

	join := machineJoinOn(t, box, endpoint, tok.Token, name, "--private")
	if join.ExitCode != 0 {
		t.Fatalf("member join on %s: exit %d\nstdout:\n%sstderr:\n%s",
			box.Alias, join.ExitCode, join.Stdout, join.Stderr)
	}
	joinedM2 = true
	if err := itest.SeedImagesTo(box, fleetImages...); err != nil {
		t.Fatalf("seed the fleet images onto %s: %v", box.Alias, err)
	}
	if err := itest.EnsureCurlOn(box); err != nil {
		t.Fatalf("curl on %s: %v", box.Alias, err)
	}

	m2 := machineNamed(t, machineList(t), name)
	if m2.Role != cfleet.RoleMember {
		t.Errorf("%s has role %q, want %q", name, m2.Role, cfleet.RoleMember)
	}
	if !m2.Private {
		t.Errorf("%s joined --private but the hub does not record it as private", name)
	}
	if !m2.Reachable(time.Now()) {
		t.Errorf("%s is not reachable right after joining: last seen %v", name, m2.LastSeen)
	}

	second := machineJoinOn(t, box, endpoint, tok.Token, name)
	if second.ExitCode != 0 {
		t.Errorf("joining twice was an error: exit %d\nstdout:\n%sstderr:\n%s",
			second.ExitCode, second.Stdout, second.Stderr)
	}
	if strings.Contains(second.Stdout, `"changed":true`) {
		t.Errorf("joining twice reported a change:\n%s", second.Stdout)
	}

	grep := onBox(t, hub, "sudo grep -c -a -F "+itest.ShellQuote(tok.Token)+
		" /var/lib/caramelo/caramelo.db || true")
	if got := strings.TrimSpace(grep.Stdout); got != "0" {
		t.Errorf("the hub's database holds the token's plaintext (%s match(es)); only its hash may be stored", got)
	}

	itest.RunGoss(t, box, itest.MustGossSpec(t, "member.yaml"))
}

func TestAnEnvironmentOnAMember(t *testing.T) {
	begin(t)
	box := needM1(t)
	name := memberNames[0]

	pushBranch(t, branch+":"+branch)

	const envName = "feat-x"
	e := createEnv(t, envName, "--on", name)
	remoteEnv = envName
	if e.Machine != name {
		t.Fatalf("env create --on %s put it on %q", name, e.Machine)
	}
	if e.VPNIP == "" {
		t.Fatal("the environment holds no address on the fleet's network")
	}

	if res := onBox(t, box, "sudo test -d "+itest.ShellQuote(e.Worktree)); res.ExitCode != 0 {
		t.Errorf("%s has no worktree at %s", box.Alias, e.Worktree)
	}
	if res := onBox(t, hub, "sudo test -d "+itest.ShellQuote(e.Worktree)); res.ExitCode == 0 {
		t.Errorf("the hub also has a worktree at %s; the environment lives on one machine", e.Worktree)
	}
	dep := cenv.ContainerName(appName, envName, dbDep)
	if names := dockerNames(t, box, "name="+dep, true); !contains(names, dep) {
		t.Errorf("%s has no %s container (it has %v)", box.Alias, dep, names)
	}
	if names := dockerNames(t, hub, "name="+dep, true); contains(names, dep) {
		t.Errorf("the hub has a %s container too", dep)
	}
	itest.RunGoss(t, box, itest.MustGossSpec(t, "member.yaml"))

	detail := showEnv(t, envName)
	if detail.Env.Machine != name {
		t.Errorf("env show says machine %q, want %q", detail.Env.Machine, name)
	}
	if got := machineShow(t, name); len(got.Envs) == 0 {
		t.Errorf("member show %s lists no environments", name)
	} else if !directoryHas(got.Envs, envName) {
		t.Errorf("member show %s does not list %s: %+v", name, envName, got.Envs)
	}

	up := mustInRepo(t, "up", envName, "--json")
	if up.ExitCode != 0 {
		t.Fatalf("up %s: exit %d\nstderr:\n%s", envName, up.ExitCode, up.Stderr)
	}
	replicas := dockerNames(t, box, "label="+cenv.LabelEnv+"="+envName, false)
	if len(replicas) == 0 {
		t.Fatalf("%s runs no container of %s after up", box.Alias, envName)
	}
	logs := mustInRepo(t, "logs", envName, "--tail", "20")
	if strings.TrimSpace(logs.Stdout) == "" {
		t.Error("logs of an environment on a member produced nothing")
	}

	execRes := mustInRepo(t, "env", "exec", envName, "--", "sh", "-c", "cat /etc/hostname")
	if strings.TrimSpace(execRes.Stdout) == "" {
		t.Error("env exec on a member produced nothing")
	}

	internalName := fmt.Sprintf("%s.%s.internal", envName, appName)
	withEnvPorts(t, envName, func(local string) {
		body := getFrom(t, local)
		if versionOf(body) == "" {
			t.Errorf("%s answered something that is not the sample app:\n%s", internalName, body)
		}
	})

	urlRes := mustInRepo(t, "env", "url", envName)
	both := urlRes.Stdout + urlRes.Stderr
	if !strings.Contains(both, internalName) && !strings.Contains(both, e.VPNIP) {
		t.Errorf("env url = %q / %q, want the tunnel name or address",
			strings.TrimSpace(urlRes.Stdout), strings.TrimSpace(urlRes.Stderr))
	}
	listed := envNamed(t, listEnvs(t, "--all"), envName)
	if listed.Machine != name {
		t.Errorf("env list --all says %s is on %q, want %q", envName, listed.Machine, name)
	}
}

func TestAPushIntoARemoteBranch(t *testing.T) {
	begin(t)
	box := needM1(t)
	needRemoteEnv(t)

	commit := setVersion(t, "2")
	push := pushBranch(t, branch+":"+remoteEnv)
	if push.ExitCode != 0 {
		t.Fatalf("push into %s: exit %d\nstderr:\n%s", remoteEnv, push.ExitCode, push.Stderr)
	}
	head := onBox(t, box, fmt.Sprintf("sudo git -c safe.directory='*' -C %s rev-parse HEAD",
		itest.ShellQuote(showEnv(t, remoteEnv).Env.Worktree)))
	if got := strings.TrimSpace(head.Stdout); got != commit {
		t.Errorf("%s's worktree is at %s, want %s", box.Alias, got, commit)
	}

	recorded := showEnv(t, remoteEnv).Env
	if recorded.Commit != commit {
		t.Errorf("%s is recorded at %q, want the commit that landed (%s)", remoteEnv, recorded.Commit, commit)
	}
	if recorded.PushedAt.IsZero() || recorded.PushedBy == "" {
		t.Errorf("the member did not write the push down: %+v", recorded)
	}
	if recorded.SourceBranch != "" {
		t.Errorf("source branch = %q; a member is never told where the hub's push came from",
			recorded.SourceBranch)
	}

	worktree := showEnv(t, remoteEnv).Env.Worktree
	onBox(t, box, fmt.Sprintf("sudo sh -c 'echo dirty >> %s/version.py'", worktree))
	before := itest.GitRefs(t, repo, commanderEnv(), hubRemote())
	setVersion(t, "3")
	refused, err := tryPushBranch(t, branch+":"+remoteEnv)
	if err != nil {
		t.Fatalf("push into a dirty worktree: %v", err)
	}
	if refused.ExitCode == 0 {
		t.Error("a push into a dirty worktree succeeded; M4 refuses it")
	}
	said := strings.ToLower(refused.Stdout + refused.Stderr)
	if !strings.Contains(said, "uncommitted") {
		t.Errorf("the refusal does not mention the uncommitted work:\n%s%s", refused.Stdout, refused.Stderr)
	}
	after := itest.GitRefs(t, repo, commanderEnv(), hubRemote())
	if after["refs/heads/"+remoteEnv] != before["refs/heads/"+remoteEnv] {
		t.Errorf("the hub's %s moved despite the refusal: %s -> %s",
			remoteEnv, before["refs/heads/"+remoteEnv], after["refs/heads/"+remoteEnv])
	}
	onBox(t, box, fmt.Sprintf("sudo git -c safe.directory='*' -C %s checkout -- .", itest.ShellQuote(worktree)))
}

func TestPlacement(t *testing.T) {
	begin(t)
	needM1(t)
	hubMachine := hubName(t)

	t.Cleanup(func() { restoreConfig(t, "caramelo.fleet.yaml") })
	t.Cleanup(func() { destroyEnv(t, "feat-y") })
	free := createEnv(t, "feat-y")
	if free.Machine == "" {
		t.Fatal("a bare env create placed the environment nowhere")
	}
	if !contains(append([]string{hubMachine}, memberNames...), free.Machine) {
		t.Errorf("feat-y landed on %q, which is not a machine of this fleet", free.Machine)
	}
	assertRoomiest(t, free.Machine)
	destroyEnv(t, "feat-y")

	if len(memberNames) > 1 && joinedM2 {
		useConfig(t, "caramelo.fleetpin.yaml")
		pushBranch(t, branch+":"+branch)
		t.Cleanup(func() { destroyEnv(t, "feat-z") })
		pinned := createEnv(t, "feat-z")
		if pinned.Machine != memberNames[1] {
			t.Errorf("feat-z landed on %q, want %q from envs.feat-z.machine", pinned.Machine, memberNames[1])
		}
		destroyEnv(t, "feat-z")
		_, res := tryCreateEnv(t, "feat-z2", "--on", memberNames[0])
		if res.ExitCode == 0 {
			t.Error("--on disagreeing with envs.<env>.machine was honoured; the file is the app's decision")
		}
		if msg := res.Stdout + res.Stderr; !strings.Contains(msg, "envs.feat-z2.machine") {
			t.Errorf("the refusal does not name the key that pins it:\n%s", msg)
		}
	}

	useConfig(t, "caramelo.fleethub.yaml")
	pushBranch(t, branch+":"+branch)
	t.Cleanup(func() { destroyEnv(t, "feat-h") })
	stayed := createEnv(t, "feat-h")
	if stayed.Machine != hubMachine {
		t.Errorf("with `placement: hub`, feat-h landed on %q, want the hub %q", stayed.Machine, hubMachine)
	}
	destroyEnv(t, "feat-h")

	useConfig(t, "caramelo.fleetbig.yaml")
	pushBranch(t, branch+":"+branch)
	_, res := tryCreateEnv(t, "feat-big")
	if res.ExitCode == 0 {
		destroyEnv(t, "feat-big")
		t.Fatal("an environment asking for 64 GiB was placed; it should have been refused")
	}
	msg := res.Stdout + res.Stderr
	for _, want := range []string{"64", hubMachine} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, msg)
		}
	}

	restoreConfig(t, "caramelo.fleet.yaml")
}

func assertRoomiest(t *testing.T, chosen string) {
	t.Helper()
	best, bestFree := "", int64(-1)
	for _, m := range machineList(t) {
		g := gaugeOf(t, m.Name)
		if g == nil {
			continue
		}
		free := g.Memory.AvailableBytes - g.Reserved.MemoryBytes
		if free > bestFree {
			best, bestFree = m.Name, free
		}
	}
	if best == "" {
		t.Log("no machine has announced a gauge; nothing to compare placement against")
		return
	}
	if best == chosen {
		return
	}
	chosenFree := int64(-1)
	if g := gaugeOf(t, chosen); g != nil {
		chosenFree = g.Memory.AvailableBytes - g.Reserved.MemoryBytes
	}
	if bestFree-chosenFree > roomTolerance {
		t.Errorf("placement chose %s with %d bytes free, but %s had %d, which is more than the %d bytes "+
			"two announcements of one kernel's memory can differ by",
			chosen, chosenFree, best, bestFree, roomTolerance)
	}
}

const roomTolerance = int64(512) << 20

func directoryHas(entries []cfleet.DirectoryEntry, name string) bool {
	for _, e := range entries {
		if e.Env == name {
			return true
		}
	}
	return false
}

func TestAReleaseAcrossTheFleet(t *testing.T) {
	begin(t)
	box := needM1(t)
	m1 := memberNames[0]

	useConfig(t, "caramelo.prod.yaml")
	pushBranch(t, branch+":"+branch)
	secretsSet(t, "production", "DB_PASSWORD=fleetpw")
	secretsSet(t, "--app-scope", "GREETING=fleet")

	prod := createEnv(t, "production", "--production", "--on", m1)
	if prod.Machine != m1 {
		t.Fatalf("production landed on %q, want %q", prod.Machine, m1)
	}
	out, _ := deploy(t, "production")
	if out.Deploy == nil || out.Deploy.Status != cenv.DeployPromoted {
		t.Fatalf("deploy production ended %v, want %s", out.Deploy, cenv.DeployPromoted)
	}

	rels := releasesOf(t)
	if len(rels) == 0 {
		t.Fatal("the hub's releases table is empty after a deploy on a member")
	}
	first := rels[0]
	if first.Machine != m1 {
		t.Errorf("the release says it was built on %q, want %q", first.Machine, m1)
	}
	wantArch := box.MustArch(t)
	if !imagesFor(first, wantArch, m1) {
		t.Errorf("release %d has no %s image on %s: %+v", first.ID, wantArch, m1, first.Images)
	}

	for _, host := range []string{"shop.test", "www.shop.test"} {
		assertServes(t, box, host)
	}

	other := memberBox(t, 1)
	if other == nil {
		t.Skip("fleet suite: this lab binds one member, so 'the image travels' has no second box")
	}
	if !joinedM2 {
		t.Skip("fleet suite: the second member never joined (an earlier case failed)")
	}
	m2 := memberNames[1]
	secretsSet(t, "staging", "DB_PASSWORD=stagingpw")
	staging := createEnv(t, "staging", "--release", "--on", m2)
	if staging.Machine != m2 {
		t.Fatalf("staging landed on %q, want %q", staging.Machine, m2)
	}

	stagingOut, stagingRes := deploy(t, "staging")
	if stagingOut.Deploy == nil || stagingOut.Deploy.Status != cenv.DeployPromoted {
		t.Fatalf("deploy staging ended %v, want %s", stagingOut.Deploy, cenv.DeployPromoted)
	}
	recordImageTransfer(t, stagingRes)
	otherArch := other.MustArch(t)
	rels = releasesOf(t)
	same := releaseWithTree(t, rels, first.Tree)
	if otherArch == wantArch {
		if !imagesFor(same, otherArch, m2) {
			t.Errorf("release %d has no %s image on %s after the deploy: %+v", same.ID, otherArch, m2, same.Images)
		}
		if imagesFor(same, otherArch, hubName(t)) {
			if !deployCopiedRatherThanBuilt(stagingOut, stagingRes.Stderr) {
				t.Errorf("the hub holds the images and staging's deploy built them again: %v",
					stepNames(stagingOut))
			}
		} else if !strings.Contains(strings.Join(stepNames(stagingOut), " "), "no machine of "+otherArch+" holds") {
			t.Errorf("only %s holds the images, so staging's deploy had to build; it does not say so: %v",
				m1, stepNames(stagingOut))
		}
	} else {
		if !imagesFor(same, otherArch, m2) {
			t.Errorf("release %d has no %s image on %s: %+v", same.ID, otherArch, m2, same.Images)
		}
		if !imagesFor(same, wantArch, m1) {
			t.Errorf("release %d lost its %s images when the %s ones were added: %+v",
				same.ID, wantArch, otherArch, same.Images)
		}
	}
	if otherArch != wantArch {
		tag := fmt.Sprintf("caramelo/%s/%s:%s", appName, webService, first.Tree)
		if imageExists(t, other, tag) && !imagesFor(same, otherArch, m2) {
			t.Errorf("%s holds %s but the directory does not say so", other.Alias, tag)
		}
	}

	setVersion(t, "4")
	pushBranch(t, branch+":"+branch)
	deploy(t, "staging")
	if _, res := rollback(t, "staging"); res.ExitCode != 0 {
		t.Errorf("rollback staging: exit %d\nstderr:\n%s", res.ExitCode, res.Stderr)
	}
}

func releaseWithTree(t *testing.T, list []releaseDoc, tree string) releaseDoc {
	t.Helper()
	for _, r := range list {
		if r.Tree == tree {
			return r
		}
	}
	t.Fatalf("no release for tree %s in %+v", tree, list)
	return releaseDoc{}
}

func imagesFor(r releaseDoc, arch, machine string) bool {
	for _, img := range r.Images {
		if img.Arch == arch && img.Machine == machine {
			return true
		}
	}
	return false
}

func deployCopiedRatherThanBuilt(out capi.DeployResult, progress string) bool {
	if strings.Contains(progress, imageCopyMark) {
		return true
	}
	for _, s := range stepNames(out) {
		if strings.Contains(s, "copy") || strings.Contains(s, "transfer") {
			return true
		}
	}
	return false
}

const imageCopyMark = "copied from"

func recordImageTransfer(t *testing.T, res itest.Result) {
	t.Helper()
	found := false
	for _, line := range strings.Split(res.Stdout+res.Stderr, "\n") {
		if strings.Contains(line, imageCopyMark) {
			t.Logf("image transfer: %s", strings.TrimSpace(line))
			found = true
		}
	}
	if !found {
		t.Log("image transfer: the deploy copied no image; it built on the machine it deployed to")
	}
}

func TestViaTheHub(t *testing.T) {
	begin(t)
	box := needSecondMember(t, "a private member fronted by the hub")
	m2 := memberNames[1]

	assertNoPublicListener(t, box)

	useConfig(t, "caramelo.fleetvia.yaml")
	pushBranch(t, branch+":"+branch)
	e := createEnv(t, "feat-p", "--on", m2)
	if e.Via != string(cedge.KindVia) && e.Via != "hub" {
		t.Errorf("feat-p was created with via %q, want hub from envs.feat-p.via", e.Via)
	}
	mustInRepo(t, "up", "feat-p", "--json")

	host := envHost("feat-p")
	assertServes(t, hub, host)

	route := routeOf(t, edgeStatusOn(t, hub).Routes, host)
	if route.Kind != cedge.KindVia {
		t.Errorf("the hub's route for %s is kind %q, want %q", host, route.Kind, cedge.KindVia)
	}
	if route.Via != m2 {
		t.Errorf("the hub's route for %s says via %q, want %q", host, route.Via, m2)
	}
	memberStatus := edgeStatusOn(t, box)
	if memberStatus.Ingress == nil || !memberStatus.Ingress.Enabled {
		t.Fatalf("%s's edge has no private ingress: %+v", box.Alias, memberStatus.Ingress)
	}
	if memberStatus.Ingress.Requests == 0 {
		t.Errorf("%s's ingress has served no request, but the hub answered for %s", box.Alias, host)
	}

	before := routeOf(t, edgeStatusOn(t, hub).Routes, host)
	load := itest.StartLoad(itest.Load{
		Client:   internet.Client(itest.Scale(30 * time.Second)),
		URL:      "https://" + host + "/",
		Interval: itest.DefaultLoadInterval,
		Marker:   versionOf,
	})
	if err := load.WaitForRequests(20, itest.Scale(30*time.Second)); err != nil {
		t.Fatalf("the load generator never got going: %v", err)
	}
	setVersion(t, "5")
	pushBranch(t, branch+":feat-p")
	mustInRepo(t, "up", "feat-p", "--json")
	report := load.Stop()
	if report.Failed > 0 {
		t.Errorf("a rollout behind the hub lost %d request(s): %s", report.Failed, report)
	}
	if flips := report.Flips(); flips != 1 {
		t.Errorf("the body switched %d time(s), want exactly 1: %v", flips, report.Markers())
	}
	after := routeOf(t, edgeStatusOn(t, hub).Routes, host)
	if !sameTargets(before, after) {
		t.Errorf("the hub's route for %s changed during a rollout on %s: %+v -> %+v",
			host, m2, before.Targets, after.Targets)
	}

	if res := inRepo(t, "env", "expose", "feat-p", "--via", "member", "--json"); res.ExitCode == 0 {
		t.Error("`env expose --via member` on a private member was accepted")
	} else if !strings.Contains(res.Stdout+res.Stderr, "private") {
		t.Errorf("the refusal does not name --private:\n%s%s", res.Stdout, res.Stderr)
	}
	if joinedM1 && remoteEnv != "" {
		m1Host := envHost(remoteEnv)
		mustInRepo(t, "env", "expose", remoteEnv, "--host", m1Host, "--via", "member", "--json")
		assertServes(t, needM1(t), m1Host)
		mustInRepo(t, "env", "expose", remoteEnv, "--host", m1Host, "--via", "hub", "--json")
		assertServes(t, hub, m1Host)
	}
}

func sameTargets(a, b cedge.Route) bool {
	if len(a.Targets) != len(b.Targets) {
		return false
	}
	for i := range a.Targets {
		if a.Targets[i].Port != b.Targets[i].Port || a.Targets[i].Replica != b.Targets[i].Replica {
			return false
		}
	}
	return true
}

func assertNoPublicListener(t *testing.T, m *itest.Machine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(time.Minute))
	defer cancel()
	addr, err := m.Address(ctx)
	if err != nil {
		t.Fatalf("the address of %s: %v", m.Alias, err)
	}
	for _, port := range []string{"80", "443"} {
		probe := fmt.Sprintf("curl -s -o /dev/null --max-time 5 --connect-timeout 5 http://%s:%s/", addr, port)
		res, err := hub.Run(ctx, probe)
		if err != nil {
			t.Fatalf("probe %s:%s from %s: %v", addr, port, hub.Alias, err)
		}
		if res.ExitCode == 0 {
			t.Errorf("%s answers on tcp %s; a --private member has no public listener at all", m.Alias, port)
		}
	}
}

func TestTheVaultIsServedToMembers(t *testing.T) {
	begin(t)
	box := needM1(t)
	needRemoteEnv(t)

	secretsSet(t, "--app-scope", "GREETING="+fleetSecret)
	mustInRepo(t, "up", remoteEnv, "--json")

	withEnvPorts(t, remoteEnv, func(local string) {
		if got := greetingOf(getFrom(t, local)); got != fleetSecret {
			t.Errorf("the greeting on %s is %q, want %q from the hub's vault", box.Alias, got, fleetSecret)
		}
	})

	if res := onBox(t, box, "sudo grep -r -l "+fleetSecret+" /var/lib/caramelo/ 2>/dev/null || true"); strings.TrimSpace(res.Stdout) != "" {
		t.Errorf("%s holds the secret's plaintext on disk: %s", box.Alias, strings.TrimSpace(res.Stdout))
	}
	if res := onBox(t, box, "sudo ls -1A /run/caramelo/secrets 2>/dev/null || true"); strings.TrimSpace(res.Stdout) != "" {
		t.Errorf("%s left an env-file behind: %s; a secret lives there for one docker run",
			box.Alias, strings.TrimSpace(res.Stdout))
	}

	feed := events(t, "--limit", "400")
	fetched, ok := eventMatching(feed, func(e cprogress.Event) bool {
		return strings.Contains(e.Action, "secret") && e.Machine == memberNames[0]
	})
	if !ok {
		t.Errorf("the hub's feed has no secret fetch attributed to %s:\n%s", memberNames[0], describeEvents(feed))
	} else {
		t.Logf("the hub recorded: %s %s by %s", fetched.Action, fetched.Status, fetched.Machine)
	}

	res := asCaramelo(t, box, itest.CarameloBinary+" secrets list --machine-scope --json")
	if res.ExitCode == 0 {
		t.Error("`secrets list` on a member's own socket answered instead of naming the hub")
	}
	if !strings.Contains(res.Stdout+res.Stderr, hubName(t)) {
		t.Errorf("the refusal does not name the hub:\n%s%s", res.Stdout, res.Stderr)
	}
}

func TestHandoff(t *testing.T) {
	begin(t)
	needM1(t)
	needRemoteEnv(t)

	me := whoAmI(t, home)
	other := secondCommander(t)
	if me == other {
		t.Fatalf("both commander HOMEs still authenticate as %q; a handoff needs two identities", me)
	}
	before := showEnv(t, remoteEnv)
	if before.Env.Owner != me {
		t.Errorf("%s is owned by %q, want its creator %q", remoteEnv, before.Env.Owner, me)
	}

	mustInRepo(t, "env", "handoff", remoteEnv, "--to", other, "--json")
	after := showEnv(t, remoteEnv)
	if after.Env.Owner != other {
		t.Fatalf("%s is owned by %q after the handoff, want %q", remoteEnv, after.Env.Owner, other)
	}

	mineHere := listEnvs(t, "--all", "--mine")
	for _, e := range mineHere {
		if e.Name == remoteEnv {
			t.Errorf("`env list --mine` on the old owner's commander still lists %s", remoteEnv)
		}
	}
	res := commander(t, commanderOpts{Dir: repo, Home: otherHome}, "env", "list", "--all", "--mine", "--json")
	if res.ExitCode != 0 {
		t.Fatalf("env list --mine on the second commander: exit %d\nstderr:\n%s", res.ExitCode, res.Stderr)
	}
	mineThere := decode[[]envDoc](t, "env list --mine", res.Stdout)
	if _, found := findEnv(mineThere, remoteEnv); !found {
		t.Errorf("`env list --mine` on the new owner's commander does not list %s: %v",
			remoteEnv, envNames(mineThere))
	}

	feed := events(t, remoteEnv, "--limit", "200")
	e, ok := eventMatching(feed, func(e cprogress.Event) bool { return e.Action == "handoff" })
	if !ok {
		t.Fatalf("no handoff event in %s's feed:\n%s", remoteEnv, describeEvents(feed))
	}
	if !strings.Contains(e.Detail, other) {
		t.Errorf("the handoff event does not name the new owner %q: %q", other, e.Detail)
	}
	if res := inRepo(t, "env", "show", remoteEnv, "--json"); res.ExitCode != 0 {
		t.Errorf("the old owner can no longer read %s: exit %d\n%s", remoteEnv, res.ExitCode, res.Stderr)
	}
}

func TestOneFeed(t *testing.T) {
	begin(t)
	needM1(t)
	needRemoteEnv(t)

	f := startFollower(t)
	deadline := time.Now().Add(itest.Scale(30 * time.Second))
	for f.Count() == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	mustInRepo(t, "up", remoteEnv, "--json")
	time.Sleep(itest.Scale(5 * time.Second))
	got := f.Stop(t)

	if len(got) == 0 {
		t.Fatal("`events --follow` on the hub carried nothing while a member ran an up")
	}
	if _, ok := eventMatching(got, func(e cprogress.Event) bool {
		return e.Machine == memberNames[0] && e.Env == remoteEnv
	}); !ok {
		t.Errorf("no event of %s on %s reached the hub's feed:\n%s",
			remoteEnv, memberNames[0], describeEvents(got))
	}
	for _, e := range got {
		if e.Machine != "" && !contains(append([]string{hubName(t)}, memberNames...), e.Machine) {
			t.Errorf("an event names machine %q, which is not in the fleet: %+v", e.Machine, e)
			break
		}
	}
}

func TestAMemberChangesAddress(t *testing.T) {
	begin(t)
	box := needM1(t)
	needRemoteEnv(t)
	name := memberNames[0]

	before := machineNamed(t, machineList(t), name)
	e := showEnv(t, remoteEnv)
	oldAddress := e.Env.VPNIP

	if box.Target() == itest.TargetDocker {
		renew := onBox(t, box, "sudo -n sh -c 'dhclient -r >/dev/null 2>&1; dhclient >/dev/null 2>&1' || true")
		if renew.ExitCode != 0 {
			t.Logf("renewing the lease on %s exited %d; the case still asserts the tunnel", box.Alias, renew.ExitCode)
		}
	} else {
		t.Logf("%s is reached over the very address this case would release, so its lease is left alone "+
			"and the case asserts that the tunnel re-handshakes after caramelod restarts", box.Alias)
	}
	restartDaemon(t, box)

	deadline := time.Now().Add(itest.Scale(3 * time.Minute))
	var after cfleet.Machine
	for time.Now().Before(deadline) {
		after = machineNamed(t, machineList(t), name)
		if after.LastSeen.After(before.LastSeen) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !after.LastSeen.After(before.LastSeen) {
		t.Fatalf("%s has not been heard from since %v; the tunnel did not re-handshake", name, before.LastSeen)
	}
	if !after.Reachable(time.Now()) {
		t.Errorf("%s reads unreachable after changing address", name)
	}

	nowShown := showEnv(t, remoteEnv)
	if nowShown.Env.VPNIP != oldAddress {
		t.Errorf("%s moved from %s to %s when its machine's lease changed",
			remoteEnv, oldAddress, nowShown.Env.VPNIP)
	}
	withEnvPorts(t, remoteEnv, func(local string) {
		if versionOf(getFrom(t, local)) == "" {
			t.Errorf("%s stopped answering after its machine changed address", remoteEnv)
		}
	})
}

func TestAMemberReboots(t *testing.T) {
	begin(t)
	box := needM1(t)
	needRemoteEnv(t)
	name := memberNames[0]

	rebootedAt := time.Now()
	cutPower(t, box)

	deadline := time.Now().Add(itest.Scale(fleetUnreachableWindow))
	var seen, back bool
	for time.Now().Before(deadline) {
		m := machineNamed(t, machineList(t), name)
		if !m.Reachable(time.Now()) {
			seen = true
			break
		}
		if m.LastSeen.After(rebootedAt.Add(cfleet.HeartbeatInterval)) {
			back = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	switch {
	case seen:
		if !stillDown(t, name) {
			t.Logf("%s came back before `env show` could ask", name)
			break
		}
		if d := showEnv(t, remoteEnv); !d.Env.Unreachable && !d.Unreachable {
			t.Errorf("env show %s does not say its machine is unreachable", remoteEnv)
		}
		if !stillDown(t, name) {
			t.Logf("%s came back before `up` could ask", name)
			break
		}
		start := time.Now()
		res := inRepo(t, "up", remoteEnv, "--json")
		switch {
		case !stillDown(t, name):
			t.Logf("%s came back while `up` was running, so what it answered is not this case's refusal", name)
		case res.ExitCode == 0:
			t.Error("`up` on an environment whose machine is rebooting succeeded")
		case !strings.Contains(res.Stdout+res.Stderr, name):
			t.Errorf("the failure does not name %s:\n%s%s", name, res.Stdout, res.Stderr)
		}
		if took := time.Since(start); took > itest.Scale(2*time.Minute) {
			t.Errorf("`up` on an unreachable machine took %s; it should fail fast", took.Round(time.Second))
		}
	case back:
		t.Logf("%s was announcing again within %s of the reboot, which is inside the %s the hub "+
			"waits before calling a member unreachable, so there was no outage to notice",
			name, time.Since(rebootedAt).Round(time.Second), cfleet.UnreachableAfter)
	default:
		t.Fatalf("%s neither read unreachable nor came back within %s", name, itest.Scale(fleetUnreachableWindow))
	}

	powerBack(t, box)
	ctx, cancel := context.WithTimeout(context.Background(), box.Budget().Boot)
	defer cancel()
	if err := itest.WaitForCaramelod(ctx, box); err != nil {
		t.Fatalf("caramelod on %s did not come back: %v", box.Alias, err)
	}
	refreshMemberPebble(t, box)

	deadline = time.Now().Add(itest.Scale(3 * time.Minute))
	for time.Now().Before(deadline) {
		if machineNamed(t, machineList(t), name).Reachable(time.Now()) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !machineNamed(t, machineList(t), name).Reachable(time.Now()) {
		t.Fatalf("%s is still unreachable after it came back; the tunnel did not re-handshake on its own", name)
	}
	if got := envNamed(t, listEnvs(t, "--all"), remoteEnv); got.Machine != name {
		t.Errorf("%s is on %q after the reboot, want %q", remoteEnv, got.Machine, name)
	}
}

func cutPower(t *testing.T, m *itest.Machine) {
	t.Helper()
	if m.Target() != itest.TargetDocker {
		t.Logf("%s is a real machine: its outage is a reboot of its own, and it may be shorter than the %s "+
			"the hub waits before calling a member unreachable", m.Alias, cfleet.UnreachableAfter)
		itest.MustRestart(t, m)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "stop", "-t", "20", m.Name).CombinedOutput()
	if err != nil {
		t.Fatalf("cut the power to %s: %v: %s", m.Alias, err, strings.TrimSpace(string(out)))
	}
}

func powerBack(t *testing.T, m *itest.Machine) {
	t.Helper()
	if m.Target() == itest.TargetDocker {
		itest.MustRestart(t, m)
	}
}

func stillDown(t *testing.T, name string) bool {
	t.Helper()
	return !machineNamed(t, machineList(t), name).Reachable(time.Now())
}

const fleetUnreachableWindow = 4 * time.Minute

func TestTheHubRestarts(t *testing.T) {
	begin(t)
	needM1(t)
	needRemoteEnv(t)

	member := needM1(t)
	refreshMemberPebble(t, member)

	host := envHost(remoteEnv)
	mustInRepo(t, "env", "expose", remoteEnv, "--host", host, "--via", "member", "--json")
	assertServes(t, member, host)

	edgeClient, err := itest.NewEdgeClient(member, rootFor(member.Alias))
	if err != nil {
		t.Fatalf("an edge client for %s: %v", member.Alias, err)
	}
	load := itest.StartLoad(itest.Load{
		Client:   edgeClient.Client(itest.Scale(30 * time.Second)),
		URL:      "https://" + host + "/",
		Interval: itest.DefaultLoadInterval,
		Marker:   versionOf,
	})
	if err := load.WaitForRequests(20, itest.Scale(30*time.Second)); err != nil {
		t.Fatalf("the load generator never got going: %v", err)
	}

	restartDaemon(t, hub)

	report := load.Stop()
	if report.Failed > 0 {
		t.Errorf("a member lost %d request(s) while the hub restarted: %s", report.Failed, report)
	}

	deadline := time.Now().Add(itest.Scale(2 * time.Minute))
	for time.Now().Before(deadline) {
		if _, found := findEnv(listEnvs(t, "--all"), remoteEnv); found {
			break
		}
		time.Sleep(5 * time.Second)
	}
	list := listEnvs(t, "--all")
	got, found := findEnv(list, remoteEnv)
	if !found {
		t.Fatalf("the directory does not hold %s after the hub restarted: %v", remoteEnv, envNames(list))
	}
	if got.Machine != memberNames[0] {
		t.Errorf("%s is on %q after the hub restarted, want %q", remoteEnv, got.Machine, memberNames[0])
	}
}

func TestTheFleetSurvivesAPowerCycle(t *testing.T) {
	begin(t)
	needM1(t)
	needRemoteEnv(t)
	name := memberNames[0]

	before := machineList(t)
	if len(before) < 2 {
		t.Fatalf("member list = %v before the power cycle, want a fleet", machineSummary(before))
	}

	for _, m := range memberBoxes {
		itest.MustRestart(t, m)
	}
	itest.MustRestart(t, hub)

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(10*time.Minute))
	defer cancel()
	if err := itest.WaitForCaramelod(ctx, hub); err != nil {
		t.Fatalf("caramelod on the hub did not come back: %v", err)
	}
	reconnect(t)
	refreshHubPebble(t)
	for _, m := range memberBoxes {
		if err := itest.WaitForCaramelod(ctx, m); err != nil {
			t.Fatalf("caramelod on %s did not come back: %v", m.Alias, err)
		}
		refreshMemberPebble(t, m)
	}

	deadline := time.Now().Add(itest.Scale(4 * time.Minute))
	var list []cfleet.Machine
	for time.Now().Before(deadline) {
		list = machineList(t)
		if len(list) == len(before) && allReachable(list) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if len(list) != len(before) {
		t.Fatalf("member list = %v after the power cycle, want %v", machineSummary(list), machineSummary(before))
	}
	if !allReachable(list) {
		t.Fatalf("not every machine is reachable after the power cycle: %v", machineSummary(list))
	}
	for _, want := range before {
		got := machineNamed(t, list, want.Name)
		if got.Subnet != want.Subnet {
			t.Errorf("%s holds %s after the power cycle, want %s", want.Name, got.Subnet, want.Subnet)
		}
	}

	got := envNamed(t, listEnvs(t, "--all"), remoteEnv)
	if got.Machine != name {
		t.Errorf("%s is on %q after the power cycle, want %q", remoteEnv, got.Machine, name)
	}
	withEnvPorts(t, remoteEnv, func(local string) {
		if versionOf(getFrom(t, local)) == "" {
			t.Errorf("%s does not answer after the whole fleet was power cycled", remoteEnv)
		}
	})
}

func allReachable(list []cfleet.Machine) bool {
	now := time.Now()
	for _, m := range list {
		if !m.Reachable(now) {
			return false
		}
	}
	return len(list) > 0
}

func TestAMemberLeaves(t *testing.T) {
	begin(t)
	box := needSecondMember(t, "removing a member that is not the one every other case uses")
	name := memberNames[1]

	held := machineShow(t, name)
	if len(held.Envs) == 0 {
		t.Skipf("fleet suite: %s holds no environment, so the refusal has nothing to name", name)
	}
	res := inRepo(t, "member", "remove", name, "--yes", "--json")
	if res.ExitCode == 0 {
		t.Fatalf("member remove %s succeeded while it held %d environment(s)", name, len(held.Envs))
	}
	for _, e := range held.Envs {
		if !strings.Contains(res.Stdout+res.Stderr, e.Env) {
			t.Errorf("the refusal does not name %s:\n%s%s", e.Env, res.Stdout, res.Stderr)
		}
	}

	subnet := machineNamed(t, machineList(t), name).Subnet
	forced := inRepo(t, "member", "remove", name, "--force", "--yes", "--json")
	if forced.ExitCode != 0 {
		t.Fatalf("member remove --force %s: exit %d\nstderr:\n%s", name, forced.ExitCode, forced.Stderr)
	}
	if machineFound(machineList(t), name) {
		t.Fatalf("%s is still in the fleet after being removed", name)
	}
	for _, e := range listEnvs(t, "--all") {
		if e.Machine == name {
			t.Errorf("the directory still holds %s on the machine that left", e.Name)
		}
	}

	for _, m := range machineList(t) {
		if m.Subnet == subnet {
			t.Errorf("%s still holds %s after %s left", m.Name, subnet, name)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(4*time.Minute))
	defer cancel()
	if err := itest.WaitForCaramelod(ctx, box); err != nil {
		t.Errorf("caramelod on %s did not survive being removed: %v", box.Alias, err)
	}
	deadline := time.Now().Add(itest.Scale(3 * time.Minute))
	var own []cfleet.Machine
	for time.Now().Before(deadline) {
		own = machineListOn(t, box)
		if len(own) == 1 && own[0].Role.IsHub() {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if len(own) != 1 || !own[0].Role.IsHub() {
		t.Errorf("%s still answers as a member after being removed: %v", box.Alias, machineSummary(own))
	}

	if res := onBox(t, box, "sudo -n "+itest.CarameloBinary+" member leave --force --json"); res.ExitCode != 0 {
		t.Errorf("member leave on %s: exit %d\n%s%s", box.Alias, res.ExitCode, res.Stdout, res.Stderr)
	}
	if err := itest.WaitForCaramelod(ctx, box); err != nil {
		t.Errorf("caramelod on %s did not come back after member leave: %v", box.Alias, err)
	}
	if role, err := itest.FleetRole(ctx, box); err != nil {
		t.Errorf("read the fleet role of %s: %v", box.Alias, err)
	} else if role == string(cfleet.RoleMember) {
		t.Errorf("%s still says it is a member of a fleet it was removed from", box.Alias)
	}
}

func machineListOn(t *testing.T, m *itest.Machine) []cfleet.Machine {
	t.Helper()
	res := asCaramelo(t, m, itest.CarameloBinary+" member list --json")
	if res.ExitCode != 0 {
		return nil
	}
	var out []cfleet.Machine
	if err := decodeInto(res.Stdout, &out); err != nil {
		return nil
	}
	return out
}

func TestTheInterface(t *testing.T) {
	begin(t)
	needM1(t)

	list := mustInRepo(t, "member", "list")
	assertPlainTable(t, "member list", list.Stdout, "NAME", "ROLE", "ARCH", "SUBNET", "ENVS", "SEEN")
	if !strings.Contains(list.Stdout, memberNames[0]) {
		t.Errorf("member list does not name %s:\n%s", memberNames[0], list.Stdout)
	}

	envs := mustInRepo(t, "env", "list", "--all")
	assertPlainTable(t, "env list --all", envs.Stdout, "NAME", "MACHINE", "OWNER")

	status := mustInRepo(t, "status")
	for _, want := range []string{"fleet", memberNames[0]} {
		if !strings.Contains(strings.ToLower(status.Stdout), strings.ToLower(want)) {
			t.Errorf("status does not mention %q:\n%s", want, status.Stdout)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), itest.Scale(2*time.Minute))
	defer cancel()
	pty, err := itest.PTYRun(ctx, repo, commanderEnv(), itest.BinaryPath(t), "member", "list")
	if err != nil {
		t.Fatalf("member list on a terminal: %v", err)
	}
	if !strings.Contains(pty.LastFrame(), memberNames[0]) {
		t.Errorf("member list on a terminal does not name %s:\n%s", memberNames[0], pty.LastFrame())
	}
}

func assertPlainTable(t *testing.T, what, out string, columns ...string) {
	t.Helper()
	header := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	for _, c := range columns {
		if !strings.Contains(strings.ToUpper(header), c) {
			t.Errorf("%s has no %s column:\n%s", what, c, out)
		}
	}
}
