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
	freshPlace(t)
	code, stdout, stderr := runWithService(t, &envService{apps: appsFixture()}, "app", "list", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, ExitOK, stderr)
	}
	if !strings.Contains(stdout, "shop") {
		t.Errorf("stdout = %q, want the applications the daemon answered with", stdout)
	}
}
