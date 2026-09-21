package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/place"
	"github.com/plytz/caramelo/internal/serverconfig"
)

func leaves(root *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if !c.HasSubCommands() {
			out = append(out, c)
			return
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	for _, sub := range root.Commands() {
		walk(sub)
	}
	return out
}

func groups(root *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if !c.HasSubCommands() {
			return
		}
		out = append(out, c)
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return out
}

func conditionalRoot(t *testing.T) *cobra.Command {
	t.Helper()
	var out bytes.Buffer
	return newRootCmd(&app{stdout: &out, stderr: &out})
}

func TestEveryLeafSaysWhenItHolds(t *testing.T) {
	root := conditionalRoot(t)
	for _, cmd := range leaves(root) {
		if _, ok := whenOf(cmd); !ok {
			t.Errorf("%s carries no available(...): every leaf says in code where it holds", cmd.CommandPath())
		}
	}
}

func TestNoGroupCarriesAConditional(t *testing.T) {
	root := conditionalRoot(t)
	for _, cmd := range groups(root) {
		if _, ok := whenOf(cmd); ok {
			t.Errorf("%s carries a conditional; a group is shown when a child of it holds", cmd.CommandPath())
		}
	}
}

func canonicalOf(t *testing.T, name string) place.Context {
	t.Helper()
	c, ok := place.Canonical(name)
	if !ok {
		t.Fatalf("no canonical context named %q", name)
	}
	return c
}

func TestEachConditionReadsTheContext(t *testing.T) {
	for _, tc := range []struct {
		name string
		w    when
		text string
		want map[string]bool
	}{
		{name: "always", w: always, text: "any machine", want: map[string]bool{
			place.CanonicalFresh: true, place.CanonicalCommander: true,
			place.CanonicalCommanderCheckout: true, place.CanonicalHub: true, place.CanonicalMember: true}},
		{name: "fresh", w: fresh, text: "a box with no config at all", want: map[string]bool{
			place.CanonicalFresh: true}},
		{name: "onCommander", w: onCommander, text: "a commander", want: map[string]bool{
			place.CanonicalCommander: true, place.CanonicalCommanderCheckout: true}},
		{name: "onHub", w: onHub, text: "a hub", want: map[string]bool{place.CanonicalHub: true}},
		{name: "onMember", w: onMember, text: "a member", want: map[string]bool{place.CanonicalMember: true}},
		{name: "onServer", w: onServer, text: "a hub or a member", want: map[string]bool{
			place.CanonicalHub: true, place.CanonicalMember: true}},
		{name: "daemonUp", w: daemonUp, text: "a machine whose caramelod is answering", want: map[string]bool{
			place.CanonicalHub: true, place.CanonicalMember: true}},
		{name: "inCheckout", w: inCheckout, text: "inside an app checkout", want: map[string]bool{
			place.CanonicalCommanderCheckout: true}},
		{name: "inWorktree", w: inWorktree, text: "inside an environment worktree", want: map[string]bool{
			place.CanonicalCommanderCheckout: true}},
		{name: "hasFleet", w: hasFleet, text: "a machine with a fleet to talk to", want: map[string]bool{
			place.CanonicalCommander: true, place.CanonicalCommanderCheckout: true,
			place.CanonicalHub: true, place.CanonicalMember: true}},
		{name: "asRoot", w: asRoot, text: "as root", want: map[string]bool{
			place.CanonicalHub: true, place.CanonicalMember: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.w.text != tc.text {
				t.Errorf("text = %q, want %q", tc.w.text, tc.text)
			}
			for _, name := range place.CanonicalNames() {
				if got := tc.w.ok(canonicalOf(t, name)); got != tc.want[name] {
					t.Errorf("on the canonical %s: %v, want %v", name, got, tc.want[name])
				}
			}
		})
	}
}

func TestTheConditionsTellThemselvesApartOffTheCanonicalFive(t *testing.T) {
	hubWithTheDaemonDown := canonicalOf(t, place.CanonicalHub)
	hubWithTheDaemonDown.Server.Services.Daemon.Present = false
	if daemonUp.ok(hubWithTheDaemonDown) {
		t.Error("daemonUp holds on a hub whose socket is not there")
	}
	if !onServer.ok(hubWithTheDaemonDown) {
		t.Error("onServer stopped holding on a hub because its daemon is down")
	}

	hubRunByAMortal := canonicalOf(t, place.CanonicalHub)
	hubRunByAMortal.Root = false
	if asRoot.ok(hubRunByAMortal) {
		t.Error("asRoot holds on a hub for a user who is not root")
	}
	freshBoxAsRoot := canonicalOf(t, place.CanonicalFresh)
	freshBoxAsRoot.Root = true
	if !asRoot.ok(freshBoxAsRoot) {
		t.Error("asRoot reads the role instead of the euid")
	}

	checkoutOutsideAWorktree := canonicalOf(t, place.CanonicalCommanderCheckout)
	checkoutOutsideAWorktree.Work.Env, checkoutOutsideAWorktree.Work.EnvFrom = "", ""
	if !inCheckout.ok(checkoutOutsideAWorktree) {
		t.Error("inCheckout stopped holding in an app checkout that is no worktree")
	}
	if inWorktree.ok(checkoutOutsideAWorktree) {
		t.Error("inWorktree holds where no environment worktree is checked out")
	}

	commanderWithNothingToTalkTo := canonicalOf(t, place.CanonicalCommander)
	commanderWithNothingToTalkTo.TalksTo = place.TalksTo{Kind: place.TalksNothing}
	if hasFleet.ok(commanderWithNothingToTalkTo) {
		t.Error("hasFleet holds on a commander with no fleet to talk to")
	}
	if fresh.ok(commanderWithNothingToTalkTo) {
		t.Error("a commander with no fleet was read as a fresh box")
	}
}

func TestConditionsComposeAndReadAsSentences(t *testing.T) {
	hub := canonicalOf(t, place.CanonicalHub)
	commander := canonicalOf(t, place.CanonicalCommander)

	both := onServer.and(asRoot)
	if both.text != "(a hub or a member), as root" {
		t.Errorf("and: text = %q", both.text)
	}
	if !both.ok(hub) || both.ok(commander) {
		t.Errorf("and: holds on the hub %v, on the commander %v", both.ok(hub), both.ok(commander))
	}

	either := onCommander.or(onServer)
	if either.text != "a commander or a hub or a member" {
		t.Errorf("or: text = %q", either.text)
	}
	if !either.ok(hub) || !either.ok(commander) {
		t.Error("or: a commander or a server should hold on both")
	}

	neither := not(onServer)
	if neither.text != "anything but (a hub or a member)" {
		t.Errorf("not: text = %q", neither.text)
	}
	if neither.ok(hub) || !neither.ok(commander) {
		t.Errorf("not: holds on the hub %v, on the commander %v", neither.ok(hub), neither.ok(commander))
	}

	nested := onCommander.and(onHub.or(onMember))
	if nested.text != "a commander, (a hub or a member)" {
		t.Errorf("nesting: text = %q", nested.text)
	}
}

func TestAvailableIsReadBackByItsText(t *testing.T) {
	cmd := available(&cobra.Command{Use: "probe"}, onServer.and(asRoot))
	if got := cmd.Annotations[whenAnnotation]; got != "(a hub or a member), as root" {
		t.Fatalf("annotation = %q", got)
	}
	w, ok := whenOf(cmd)
	if !ok {
		t.Fatal("the conditional cannot be read back from the command")
	}
	if !w.ok(canonicalOf(t, place.CanonicalHub)) {
		t.Error("the conditional read back is not the one that was written")
	}
	if cmd.Annotations["kind"] != "" {
		t.Error("available touched the kind annotation, which decides forwarding")
	}
}

func hubPlace(t *testing.T) {
	t.Helper()
	serverPlace(t, "name: box\nrole: hub\nhub:\n  fleet: home\n")
}

func memberPlace(t *testing.T) {
	t.Helper()
	serverPlace(t, "name: worker\nrole: member\nmember:\n  fleet: home\n  hub:\n"+
		"    endpoint: box.example.com:4021\n    public_key: Nq0Xw2mS8VbZ1YtR7dK3jL5pQ9cF4hG6uI8oP0aB2wE=\n")
}

func serverPlace(t *testing.T, body string) {
	t.Helper()
	freshPlace(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, serverconfig.ConfigFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	useSystemConfigDir(t, dir)
}

func commanderPlace(t *testing.T) {
	t.Helper()
	freshPlace(t)
	useCommanderConfig(t, oneFleet())
}

func TestALeafTypedWhereItDoesNotBelongIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		where func(t *testing.T)
		args  []string
		want  string
	}{
		{name: "vpn up on a fresh box", where: freshPlace, args: []string{"vpn", "up"},
			want: "vpn up runs on a commander, and this box has no config at all; " +
				"run 'caramelo commander init' to name this machine"},
		{name: "hub run on a commander", where: commanderPlace, args: []string{"hub", "run"},
			want: "hub run runs on a hub or a member, and this machine is a commander; " +
				"run 'sudo caramelo hub setup' to make this machine a hub"},
		{name: "fleet list on a hub", where: hubPlace, args: []string{"fleet", "list"},
			want: "fleet list runs on a commander, and this machine is a hub; " +
				"'caramelo context' says where you are"},
		{name: "vpn up on a member", where: memberPlace, args: []string{"vpn", "up"},
			want: "vpn up runs on a commander, and this machine is a member; " +
				"'caramelo context' says where you are"},
		{name: "member leave on a hub", where: hubPlace, args: []string{"member", "leave"},
			want: "member leave runs on a member, and this machine is a hub; " +
				"'caramelo context' says where you are"},
		{name: "member join on a fresh box", where: freshPlace, args: []string{"member", "join", "box:4021"},
			want: "member join runs on a hub or a member, and this box has no config at all; " +
				"run 'sudo caramelo hub setup' to make this machine a hub"},
		{name: "member join on a commander", where: commanderPlace, args: []string{"member", "join", "box:4021"},
			want: "member join runs on a hub or a member, and this machine is a commander; " +
				"run 'sudo caramelo hub setup' to make this machine a hub"},
		{name: "env sync on a fresh box", where: freshPlace, args: []string{"env", "sync", "feat-x"},
			want: "env sync runs on a hub or a member, and this box has no config at all; " +
				"run 'sudo caramelo hub setup' to make this machine a hub"},
		{name: "commander init on a member", where: memberPlace, args: []string{"commander", "init"},
			want: "commander init runs on a box with no config at all or a commander, " +
				"and this machine is a member; 'caramelo context' says where you are"},
		{name: "up on a member", where: memberPlace, args: []string{"up"},
			want: "up runs on a commander or a hub, and this machine is a member; " +
				"'caramelo context' says where you are"},
		{name: "secrets list on a fresh box", where: freshPlace, args: []string{"secrets", "list"},
			want: "secrets list runs on a commander or a hub or a member, and this box has no config at all; " +
				"run 'caramelo commander init' to name this machine"},
		{name: "config show on a member", where: memberPlace, args: []string{"config", "show"},
			want: "config show runs on a commander or a hub, and this machine is a member; " +
				"'caramelo context' says where you are"},
		{name: "key add on a member", where: memberPlace, args: []string{"key", "add", "--name", "alex"},
			want: "key add runs on a commander or a hub, and this machine is a member; " +
				"'caramelo context' says where you are"},
		{name: "member remove on a member", where: memberPlace, args: []string{"member", "remove", "worker"},
			want: "member remove runs on a commander or a hub, and this machine is a member; " +
				"'caramelo context' says where you are"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.where(t)
			fwd := &fakeForward{}
			fwd.install(t)

			code, stdout, stderr := run(t, tc.args...)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d", code, ExitUsage)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing; the refusal belongs on stderr", stdout)
			}
			if stderr != "caramelo: "+tc.want+"\n" {
				t.Errorf("stderr = %q, want one line %q", stderr, "caramelo: "+tc.want)
			}
			if fwd.called {
				t.Error("the command was forwarded although it does not belong here")
			}
		})
	}
}

func preRunOwner(cmd *cobra.Command) *cobra.Command {
	for c := cmd; c != nil; c = c.Parent() {
		if c.PersistentPreRunE != nil {
			return c
		}
	}
	return nil
}

func preRunOwners(root *cobra.Command) []string {
	var out []string
	for _, cmd := range append(groups(root), leaves(root)...) {
		if cmd.PersistentPreRunE != nil {
			out = append(out, cmd.CommandPath())
		}
	}
	return out
}

func TestEveryPreRunHookRefusesALeafThatDoesNotHold(t *testing.T) {
	asked := map[string]bool{}
	for _, tc := range []struct {
		name  string
		where func(t *testing.T)
	}{
		{name: "on a fresh box", where: freshPlace},
		{name: "on a commander", where: commanderPlace},
		{name: "on a hub", where: hubPlace},
		{name: "on a member", where: memberPlace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.where(t)
			var out bytes.Buffer
			a := &app{stdout: &out, stderr: &out}
			root := newRootCmd(a)
			c, err := a.place(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, leaf := range leaves(root) {
				w, ok := whenOf(leaf)
				if !ok || w.ok(c) {
					continue
				}
				owner := preRunOwner(leaf)
				if owner == nil {
					t.Errorf("%s has no pre-run hook above it, so nothing refuses it", leaf.CommandPath())
					continue
				}
				asked[owner.CommandPath()] = true
				var notHere *notHereError
				if err := owner.PersistentPreRunE(leaf, nil); !errors.As(err, &notHere) {
					t.Errorf("%s: the hook on %s returned %v, want the refusal",
						leaf.CommandPath(), owner.CommandPath(), err)
				}
			}
		})
	}
	for _, owner := range preRunOwners(conditionalRoot(t)) {
		if !asked[owner] {
			t.Errorf("no case reaches the pre-run hook on %s, so it could stop refusing unnoticed", owner)
		}
	}
}

func TestALeafTypedWhereItBelongsRuns(t *testing.T) {
	commanderPlace(t)
	fwd := &fakeForward{}
	fwd.install(t)
	if code, _, stderr := run(t, "status"); code != ExitOK {
		t.Fatalf("status on a commander: exit %d: %s", code, stderr)
	}
	if !fwd.called {
		t.Error("status on a commander was not forwarded")
	}
}

func TestTheHelpOfALeafThatDoesNotHoldHereStillPrints(t *testing.T) {
	for _, tc := range []struct {
		name  string
		where func(t *testing.T)
		args  []string
		want  string
	}{
		{name: "hub run on a commander", where: commanderPlace,
			args: []string{"hub", "run"}, want: "caramelod"},
		{name: "member join on a fresh box", where: freshPlace,
			args: []string{"member", "join"}, want: "caramelo hub setup"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.where(t)
			code, stdout, stderr := run(t, append(tc.args, "--help")...)
			if code != ExitOK {
				t.Fatalf("exit = %d: %s", code, stderr)
			}
			if !strings.Contains(stdout, tc.want) {
				t.Errorf("stdout = %q, want the help of %s", stdout, strings.Join(tc.args, " "))
			}
		})
	}
}

func TestAPlaceThatCannotBeReadRefusesNothing(t *testing.T) {
	var out bytes.Buffer
	a := &app{stdout: &out, stderr: &out}
	a.placeOnce.Do(func() { a.placeErr = errors.New("locate the commander config: no home directory") })
	cmd := available(&cobra.Command{Use: "run"}, onServer)
	if err := a.refuseWhereItDoesNotBelong(cmd); err != nil {
		t.Errorf("a place that could not be read refused the command: %v", err)
	}
}

func TestAConfigThatCannotBeInterpretedRefusesNothing(t *testing.T) {
	serverPlace(t, "fleet:\n  role: hub\n")
	var out bytes.Buffer
	a := &app{stdout: &out, stderr: &out}
	c, err := a.place(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.Role != place.RoleUnknown || c.Problem == "" {
		t.Fatalf("role = %q, problem = %q, want a config that cannot be read", c.Role, c.Problem)
	}
	for _, w := range []when{fresh, onCommander, onServer, not(onCommander)} {
		if err := a.refuseWhereItDoesNotBelong(available(&cobra.Command{Use: "setup"}, w)); err != nil {
			t.Errorf("%q refused the command that would report the problem: %v", w.text, err)
		}
	}
}

func TestASessionTheDaemonRunsIsNeverRefused(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	freshPlace(t)
	code, stdout, stderr := runWithService(t, &envService{apps: appsFixture()}, "app", "list", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitOK, stderr)
	}
	if !strings.Contains(stdout, "shop") {
		t.Errorf("stdout = %q, want the applications the daemon answered with", stdout)
	}
}

func TestADormantLeafIsRefusedNamingTheSwitch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		where func(t *testing.T)
		args  []string
		want  string
	}{
		{name: "fleet list on a commander", where: commanderPlace, args: []string{"fleet", "list"},
			want: "fleet list is not approved on a commander yet; " +
				"set CARAMELO_EXPERIMENTAL=1 to run a dormant command"},
		{name: "hub status on a hub", where: hubPlace, args: []string{"hub", "status"},
			want: "hub status is not approved on a hub or a member yet; " +
				"set CARAMELO_EXPERIMENTAL=1 to run a dormant command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(experimentalEnv, "")
			tc.where(t)
			fwd := &fakeForward{}
			fwd.install(t)

			code, stdout, stderr := run(t, tc.args...)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d", code, ExitUsage)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing; the refusal belongs on stderr", stdout)
			}
			if stderr != "caramelo: "+tc.want+"\n" {
				t.Errorf("stderr = %q, want one line %q", stderr, "caramelo: "+tc.want)
			}
			if fwd.called {
				t.Error("a dormant command was forwarded")
			}
		})
	}
}

func TestTheSwitchLiftsTheGate(t *testing.T) {
	t.Setenv(experimentalEnv, "1")
	commanderPlace(t)
	fwd := &fakeForward{}
	fwd.install(t)
	if code, _, stderr := run(t, "status"); code != ExitOK {
		t.Fatalf("status with %s set: exit %d: %s", experimentalEnv, code, stderr)
	}
	if !fwd.called {
		t.Error("status was not forwarded although the switch lifts the gate")
	}
}

func approveOnCommander(t *testing.T, names ...string) {
	t.Helper()
	old := approvedOnCommander
	approvedOnCommander = names
	t.Cleanup(func() { approvedOnCommander = old })
}

func approveOnServer(t *testing.T, names ...string) {
	t.Helper()
	old := approvedOnServer
	approvedOnServer = names
	t.Cleanup(func() { approvedOnServer = old })
}

func TestALeafOnItsListRunsWithTheGateDown(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	commanderPlace(t)
	approveOnCommander(t, "status")
	fwd := &fakeForward{}
	fwd.install(t)

	if code, _, stderr := run(t, "status"); code != ExitOK {
		t.Fatalf("status, which is on the commander list: exit %d: %s", code, stderr)
	}
	if !fwd.called {
		t.Error("a leaf on its list was not forwarded")
	}
	if code, _, _ := run(t, "context"); code != ExitUsage {
		t.Errorf("context: exit %d, want %d; only what is on the list runs", code, ExitUsage)
	}
}

func TestThePlaceRefusalComesBeforeTheDormantOne(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	hubPlace(t)
	code, _, stderr := run(t, "fleet", "list")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	want := "caramelo: fleet list runs on a commander, and this machine is a hub; " +
		"'caramelo context' says where you are\n"
	if stderr != want {
		t.Errorf("stderr = %q, want the place refusal %q", stderr, want)
	}
}

func TestAListIsPickedByThePlace(t *testing.T) {
	approveOnCommander(t, "on the commander list")
	approveOnServer(t, "on the server list")
	for _, tc := range []struct {
		name  string
		where string
		marks string
	}{
		{name: place.CanonicalFresh, where: "a commander", marks: "on the commander list"},
		{name: place.CanonicalCommander, where: "a commander", marks: "on the commander list"},
		{name: place.CanonicalCommanderCheckout, where: "a commander", marks: "on the commander list"},
		{name: place.CanonicalHub, where: "a hub or a member", marks: "on the server list"},
		{name: place.CanonicalMember, where: "a hub or a member", marks: "on the server list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ap, ok := approvalFor(canonicalOf(t, tc.name))
			if !ok {
				t.Fatalf("the canonical %s is judged by no list", tc.name)
			}
			if ap.where != tc.where {
				t.Errorf("where = %q, want %q", ap.where, tc.where)
			}
			if !ap.covers(tc.marks) {
				t.Errorf("the list of the canonical %s is not the one that says %q", tc.name, tc.marks)
			}
		})
	}
	unreadable := canonicalOf(t, place.CanonicalHub)
	unreadable.Role = place.RoleUnknown
	if unreadable.Server == nil {
		t.Fatal("a server whose config cannot be interpreted still carries a Server; the shape under test is stale")
	}
	if ap, ok := approvalFor(unreadable); ok {
		t.Errorf("a machine whose role cannot be read is judged by %q", ap.where)
	}
}

func TestEveryLeafResolvesToExactlyOneList(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	cmds := leaves(conditionalRoot(t))
	for _, name := range place.CanonicalNames() {
		t.Run(name, func(t *testing.T) {
			c := canonicalOf(t, name)
			ap, ok := approvalFor(c)
			if !ok {
				t.Fatalf("the canonical %s is judged by no list", name)
			}
			byCommander := ap.where == "a commander"
			byServer := ap.where == "a hub or a member"
			if byCommander == byServer {
				t.Fatalf("the canonical %s is judged by %q, which is neither list", name, ap.where)
			}
			if byServer != c.IsServer() || byCommander != (c.IsFresh() || c.IsCommander()) {
				t.Errorf("the canonical %s is judged by %q; the place picks the list", name, ap.where)
			}
			for _, cmd := range cmds {
				if isMachinery(cmd) || ap.covers(leafName(cmd)) {
					continue
				}
				got, dormant := dormantHere(cmd, c)
				if !dormant {
					t.Errorf("%s is not dormant on the canonical %s although it is on no list there",
						leafName(cmd), name)
					continue
				}
				if got.where != ap.where {
					t.Errorf("%s on the canonical %s is judged by %q, the place by %q",
						leafName(cmd), name, got.where, ap.where)
				}
			}
		})
	}
}

func TestMachineryIsNeverDormant(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	var seen, direct, viaAncestor int
	for _, cmd := range leaves(conditionalRoot(t)) {
		if !isMachinery(cmd) {
			continue
		}
		seen++
		if cmd.Hidden {
			direct++
		} else {
			viaAncestor++
		}
		for _, name := range place.CanonicalNames() {
			if ap, dormant := dormantHere(cmd, canonicalOf(t, name)); dormant {
				t.Errorf("%s is machinery and was judged against %q on the canonical %s",
					leafName(cmd), ap.where, name)
			}
		}
	}
	if seen == 0 {
		t.Error("no machinery leaf was found; the hidden leaves of the tree are gone")
	}
	t.Logf("machinery: %d leaves hidden themselves, %d under a hidden parent", direct, viaAncestor)
}

func TestAChildOfAHiddenGroupIsMachinery(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	root := &cobra.Command{Use: "caramelo"}
	parent := &cobra.Command{Use: "parent", Hidden: true}
	child := available(&cobra.Command{Use: "child"}, onServer)
	parent.AddCommand(child)
	root.AddCommand(parent)
	if !isMachinery(child) {
		t.Fatal("a leaf under a hidden group is not machinery; the walk up to a hidden parent stopped working")
	}
	c := canonicalOf(t, place.CanonicalHub)
	if ap, dormant := dormantHere(child, c); dormant {
		t.Errorf("a leaf under a hidden group was judged against %q", ap.where)
	}
	if !shownHere(child, c) {
		t.Error("a leaf under a hidden group is hidden by the approval gate")
	}
}

func TestADormantLeafIsNotRefusedWhereThePlaceCannotBeRead(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	serverPlace(t, "fleet:\n  role: hub\n")
	var out bytes.Buffer
	a := &app{stdout: &out, stderr: &out}
	c, err := a.place(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.Role != place.RoleUnknown {
		t.Fatalf("role = %q, want a config that cannot be read", c.Role)
	}
	if err := a.refuseWhereItDoesNotBelong(available(&cobra.Command{Use: "setup"}, always)); err != nil {
		t.Errorf("a config that cannot be interpreted refused a dormant command: %v", err)
	}
	for _, cmd := range leaves(conditionalRoot(t)) {
		w, ok := whenOf(cmd)
		if isMachinery(cmd) || !ok || !w.ok(c) {
			continue
		}
		if !shownHere(cmd, c) {
			t.Errorf("%s runs where the config cannot be interpreted and is hidden there", leafName(cmd))
		}
	}
	if help := helpIn(t, c, ""); !lists(help, "context") {
		t.Errorf("the help where the config cannot be interpreted leaves out context:\n%s", help)
	}
	m := buildManual(manualRoot(t), &manualScope{role: c.Role, ctx: c, here: true})
	documented := map[string]bool{}
	for _, path := range manualPaths(m) {
		documented[path] = true
	}
	if !documented["caramelo context"] {
		t.Errorf("the manual where the config cannot be interpreted documents %v, without context",
			manualPaths(m))
	}

	var unreadable bytes.Buffer
	b := &app{stdout: &unreadable, stderr: &unreadable}
	b.placeOnce.Do(func() { b.placeErr = errors.New("locate the commander config: no home directory") })
	if err := b.refuseWhereItDoesNotBelong(available(&cobra.Command{Use: "probe"}, always)); err != nil {
		t.Errorf("a place that could not be read refused a dormant command: %v", err)
	}
}

func TestTheHelpOfADormantLeafStillPrints(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	commanderPlace(t)
	code, stdout, stderr := run(t, "fleet", "list", "--help")
	if code != ExitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "list shows every fleet in the commander config") {
		t.Errorf("stdout = %q, want the help of fleet list", stdout)
	}
}

func TestEveryApprovedNameIsALeafOfTheTree(t *testing.T) {
	known := map[string]bool{}
	for _, cmd := range leaves(conditionalRoot(t)) {
		if isMachinery(cmd) {
			continue
		}
		known[leafName(cmd)] = true
	}
	for _, tc := range []struct {
		list  string
		names []string
	}{
		{list: "approvedOnCommander", names: approvedOnCommander},
		{list: "approvedOnServer", names: approvedOnServer},
	} {
		for _, name := range tc.names {
			if !known[name] {
				t.Errorf("%s names %q, which is no leaf of the tree, or is machinery", tc.list, name)
			}
		}
	}
}

func TestTheServerListCoversWhatASetupTypesOnTheBox(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	root := conditionalRoot(t)
	for _, tc := range []struct {
		name string
		args []string
		site string
	}{
		{name: "the caramelod smoke test", args: []string{"status"},
			site: "internal/setup/steps_caramelod.go:518, on every setup, and " +
				"internal/vpnclient/client.go:278 through the shellControl of " +
				"internal/cli/hub_setup_target.go:473"},
		{name: "the join step", args: []string{"member", "join"},
			site: "internal/setup/steps_fleet.go:53, on a setup that joins a fleet"},
		{name: "the peer step admits the commander", args: []string{"peer", "add"},
			site: "internal/setup/steps_vpn.go:138, and internal/vpnclient/client.go:294 " +
				"through the shellControl of internal/cli/hub_setup_target.go:473"},
		{name: "the peer step reads the peers", args: []string{"peer", "list"},
			site: "internal/setup/steps_vpn.go:157"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, _, err := root.Find(tc.args)
			if err != nil {
				t.Fatalf("caramelo %s: %v", strings.Join(tc.args, " "), err)
			}
			for _, name := range []string{place.CanonicalHub, place.CanonicalMember} {
				c := canonicalOf(t, name)
				if ap, dormant := dormantHere(cmd, c); dormant {
					t.Errorf("%s is dormant on the canonical %s, judged by %q; the setup types it "+
						"on the box itself (%s), so the setup dies there",
						leafName(cmd), name, ap.where, tc.site)
				}
			}
		})
	}
}

func TestTheCommanderListCoversTheSetupOfABareBox(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	root := conditionalRoot(t)
	c := canonicalOf(t, place.CanonicalFresh)
	for _, args := range [][]string{{"commander", "init"}, {"hub", "setup"}} {
		cmd, _, err := root.Find(args)
		if err != nil {
			t.Fatalf("caramelo %s: %v", strings.Join(args, " "), err)
		}
		if ap, dormant := dormantHere(cmd, c); dormant {
			t.Errorf("%s is dormant on a box with no config at all, judged by %q; a bare box is "+
				"judged by the commander list, and naming a commander and setting a machine up "+
				"are what is typed on one",
				leafName(cmd), ap.where)
		}
	}
}

func TestTheCommanderListCoversTheTunnelClient(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	root := conditionalRoot(t)
	c := canonicalOf(t, place.CanonicalCommander)
	for _, args := range [][]string{{"vpn", "up"}, {"vpn", "down"}, {"vpn", "status"}} {
		cmd, _, err := root.Find(args)
		if err != nil {
			t.Fatalf("caramelo %s: %v", strings.Join(args, " "), err)
		}
		if ap, dormant := dormantHere(cmd, c); dormant {
			t.Errorf("%s is dormant on a commander, judged by %q; the Go WireGuard client is "+
				"approved on a commander as one set, up, down and status together",
				leafName(cmd), ap.where)
		}
	}
}
