package cli

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/plytz/caramelo/internal/place"
)

func helpIn(t *testing.T, c place.Context, path string) string {
	t.Helper()
	var out bytes.Buffer
	a := &app{stdout: &out, stderr: &out}
	a.placeOnce.Do(func() { a.placeHere = c })
	root := newRootCmd(a)
	root.InitDefaultHelpCmd()
	cmd := root
	if path != "" {
		found, _, err := root.Find(strings.Fields(path))
		if err != nil {
			t.Fatalf("caramelo %s: %v", path, err)
		}
		cmd = found
	}
	cmd.InitDefaultHelpFlag()
	if err := cmd.Help(); err != nil {
		t.Fatalf("caramelo %s --help: %v", path, err)
	}
	return out.String()
}

func helpGroups() []string {
	return []string{"hub", "member", "env", "edge", "vpn", "secrets", "key", "peer", "task"}
}

func TestGoldenHelpOfEveryPlace(t *testing.T) {
	for _, name := range place.CanonicalNames() {
		c := canonicalOf(t, name)
		for _, path := range append([]string{""}, helpGroups()...) {
			where := path
			if where == "" {
				where = "root"
			}
			t.Run(name+"-"+where, func(t *testing.T) {
				checkGolden(t, "help-"+name+"-"+where, helpIn(t, c, path))
			})
		}
	}
}

func listedCommands(out string) []string {
	var names []string
	listing := false
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "Available Commands:"):
			listing = true
		case !listing:
		case strings.TrimSpace(line) == "":
			listing = false
		default:
			names = append(names, strings.Fields(line)[0])
		}
	}
	return names
}

func lists(out, name string) bool {
	for _, got := range listedCommands(out) {
		if got == name {
			return true
		}
	}
	return false
}

func TestHelpOpensWithTheHeaderOfThePlace(t *testing.T) {
	for _, tc := range []struct {
		name  string
		where func(t *testing.T)
		want  string
	}{
		{name: "fresh", where: freshPlace, want: "fresh box: no config · run 'caramelo commander init'"},
		{name: "commander", where: commanderPlace, want: "laptop, commander · fleets home (default)"},
		{name: "hub", where: hubPlace, want: "box, hub of fleet home ·"},
		{name: "member", where: memberPlace, want: "worker, member of fleet home (hub box.example.com:4021) ·"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.where(t)
			code, stdout, stderr := run(t, "--help")
			if code != ExitOK {
				t.Fatalf("exit = %d: %s", code, stderr)
			}
			if !strings.HasPrefix(stdout, tc.want) {
				t.Errorf("help does not open with the header of this place, want it to start with %q:\n%s",
					tc.want, stdout)
			}
			head := strings.SplitN(stdout, "\n\n", 2)
			if len(head) != 2 {
				t.Fatalf("no blank line after the header:\n%s", stdout)
			}
			if n := strings.Count(head[0], "\n") + 1; n > 3 {
				t.Errorf("the header is %d lines, want at most three:\n%s", n, head[0])
			}
		})
	}
}

func TestHelpListsOnlyTheCommandsThatHoldHere(t *testing.T) {
	for _, tc := range []struct {
		name   string
		where  func(t *testing.T)
		args   []string
		shown  []string
		hidden []string
	}{
		{name: "fresh", where: freshPlace,
			shown:  []string{"commander", "context", "hub", "manual", "version"},
			hidden: []string{"env", "up", "deploy", "fleet", "vpn", "member", "secrets", "key", "peer", "status"}},
		{name: "commander", where: commanderPlace,
			shown: []string{"commander", "env", "up", "deploy", "fleet", "vpn", "hub", "member",
				"secrets", "key", "peer", "edge"}},
		{name: "commander-hub", where: commanderPlace, args: []string{"hub"},
			shown:  []string{"setup", "probe"},
			hidden: []string{"status", "uninstall", "run"}},
		{name: "hub", where: hubPlace,
			shown:  []string{"hub", "member", "env", "up", "secrets", "edge", "key", "peer"},
			hidden: []string{"commander", "fleet", "vpn"}},
		{name: "member", where: memberPlace,
			shown:  []string{"hub", "member", "edge", "secrets", "peer", "status", "events"},
			hidden: []string{"commander", "fleet", "vpn", "env", "up", "down", "deploy", "key", "connect"}},
		{name: "member-member", where: memberPlace, args: []string{"member"},
			shown:  []string{"join", "leave", "list", "show"},
			hidden: []string{"add", "remove", "token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.where(t)
			code, stdout, stderr := run(t, append(tc.args, "--help")...)
			if code != ExitOK {
				t.Fatalf("exit = %d: %s", code, stderr)
			}
			for _, name := range tc.shown {
				if !lists(stdout, name) {
					t.Errorf("%s is not listed on a %s:\n%s", name, tc.name, stdout)
				}
			}
			for _, name := range tc.hidden {
				if lists(stdout, name) {
					t.Errorf("%s is listed on a %s, where it does not hold:\n%s", name, tc.name, stdout)
				}
			}
		})
	}
}

func TestAGroupIsHiddenOnceNoChildOfItHolds(t *testing.T) {
	memberPlace(t)
	code, stdout, stderr := run(t, "env", "--help")
	if code != ExitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if names := listedCommands(stdout); len(names) != 0 {
		t.Errorf("env lists %v on a member, where none of them holds", names)
	}
	if !strings.Contains(stdout, "An environment is an isolated") {
		t.Errorf("the help of the group itself did not print:\n%s", stdout)
	}
	if !strings.Contains(stdout, "hidden here") {
		t.Errorf("nothing says the group is empty here:\n%s", stdout)
	}
	if lists(helpIn(t, canonicalOf(t, place.CanonicalMember), ""), "env") {
		t.Error("the env group is listed at the root although no child of it holds on a member")
	}
}

func hiddenByThePlace(t *testing.T, c place.Context) []string {
	t.Helper()
	var names []string
	for _, cmd := range leaves(conditionalRoot(t)) {
		if isMachinery(cmd) {
			continue
		}
		if w, ok := whenOf(cmd); ok && !w.ok(c) {
			names = append(names, leafName(cmd))
		}
	}
	return names
}

func TestTheLineAfterTheListCountsWhatIsHiddenHere(t *testing.T) {
	for _, name := range place.CanonicalNames() {
		t.Run(name, func(t *testing.T) {
			c := canonicalOf(t, name)
			out := helpIn(t, c, "")
			want := hiddenByThePlace(t, c)
			if len(want) == 0 {
				t.Fatalf("nothing is hidden on the canonical %s; the count cannot be checked", name)
			}
			line := strconv.Itoa(len(want)) +
				" commands are hidden here; 'caramelo manual --role all' lists every command of every role."
			if !strings.HasSuffix(out, "\n"+line+"\n") {
				t.Errorf("help does not end with %q:\n%s", line, out)
			}
		})
	}
}

func TestTheCountIsSingularForOneHiddenCommand(t *testing.T) {
	want := "1 command is hidden here; 'caramelo manual --role all' lists every command of every role."
	if got := hiddenHere(1); got != want {
		t.Errorf("hiddenHere(1) = %q, want %q", got, want)
	}
}

func TestNothingIsSaidWhenEveryCommandHoldsHere(t *testing.T) {
	out := helpIn(t, canonicalOf(t, place.CanonicalCommander), "fleet")
	if strings.Contains(out, "hidden here") {
		t.Errorf("the fleet group counted hidden commands on a commander, where all four hold:\n%s", out)
	}
}

func TestTheHelpOfALeafCarriesNoHeaderAndNoCount(t *testing.T) {
	commanderPlace(t)
	code, stdout, stderr := run(t, "hub", "run", "--help")
	if code != ExitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if strings.Contains(stdout, "hidden here") {
		t.Errorf("a leaf's help counted hidden commands:\n%s", stdout)
	}
	if strings.Contains(strings.SplitN(stdout, "\n", 2)[0], " · ") {
		t.Errorf("a leaf's help opens with the header of the place:\n%s", stdout)
	}
}

func TestHelpHasNoRoleFlag(t *testing.T) {
	commanderPlace(t)
	code, stdout, _ := run(t, "--help")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, line := range strings.Split(stdout, "\n") {
		flag := strings.TrimSpace(line)
		if strings.HasPrefix(flag, "--role") || strings.Contains(flag, ", --role") {
			t.Errorf("help offers a --role flag; it shows the place it is in:\n%s", stdout)
		}
	}
	if code, _, _ := run(t, "--help", "--role", "hub"); code != ExitUsage {
		t.Errorf("caramelo --help --role hub: exit %d, want %d", code, ExitUsage)
	}
}

func TestTheHeaderOfEveryPlaceIsOneToThreeLines(t *testing.T) {
	for _, name := range place.CanonicalNames() {
		c := canonicalOf(t, name)
		lines := headerLines(c.HeaderParts())
		if len(lines) == 0 || len(lines) > 3 {
			t.Errorf("the header of the canonical %s is %d lines:\n%s", name, len(lines),
				strings.Join(lines, "\n"))
			continue
		}
		for _, line := range lines {
			if strings.TrimSpace(line) == "" {
				t.Errorf("the header of the canonical %s has an empty line", name)
			}
		}
		if !strings.Contains(lines[0], c.Name) {
			t.Errorf("the header of the canonical %s does not open with the machine's name: %q", name, lines[0])
		}
	}
}

func TestTheHeaderWrapsOnItsOwnSeparators(t *testing.T) {
	long := strings.Repeat("x", 70)
	lines := headerLines([]string{"box, hub of fleet home", long, "edge on"})
	if len(lines) != 2 {
		t.Fatalf("lines = %v, want two", lines)
	}
	if !strings.HasSuffix(lines[0], " ·") {
		t.Errorf("a wrapped line does not end on its separator: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], headerIndent+long) {
		t.Errorf("the second line is not indented under the first: %q", lines[1])
	}
}

func TestTheHeaderNeverGrowsPastThreeLines(t *testing.T) {
	long := strings.Repeat("x", 70)
	parts := []string{"box, hub of fleet home", long, long, long, long, "edge on"}
	lines := headerLines(parts)
	if len(lines) != headerMaxLines {
		t.Fatalf("lines = %d, want %d:\n%s", len(lines), headerMaxLines, strings.Join(lines, "\n"))
	}
	for _, part := range parts {
		if !strings.Contains(strings.Join(lines, "\n"), part) {
			t.Errorf("%q is missing from the header:\n%s", part, strings.Join(lines, "\n"))
		}
	}
	if !strings.HasSuffix(lines[headerMaxLines-1], "edge on") {
		t.Errorf("the last line does not end on the last part: %q", lines[headerMaxLines-1])
	}
}

func TestAPlaceThatCannotBeReadHidesNothing(t *testing.T) {
	var out bytes.Buffer
	a := &app{stdout: &out, stderr: &out}
	a.placeOnce.Do(func() { a.placeErr = errors.New("locate the commander config: no home directory") })
	root := newRootCmd(a)
	if err := root.Help(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"vpn", "fleet", "env", "commander", "hub", "member"} {
		if !lists(out.String(), name) {
			t.Errorf("%s is missing although the place could not be read:\n%s", name, out.String())
		}
	}
	if strings.Contains(out.String(), "hidden here") {
		t.Errorf("a place that could not be read counted hidden commands:\n%s", out.String())
	}
}

func TestHelpLeavesTheTreeAsItFoundIt(t *testing.T) {
	var out bytes.Buffer
	a := &app{stdout: &out, stderr: &out}
	a.placeOnce.Do(func() { a.placeHere = canonicalOf(t, place.CanonicalMember) })
	root := newRootCmd(a)
	before := map[string]bool{}
	for _, cmd := range append(leaves(root), groups(root)...) {
		before[cmd.CommandPath()] = cmd.Hidden
	}
	if err := root.Help(); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range append(leaves(root), groups(root)...) {
		if cmd.Hidden != before[cmd.CommandPath()] {
			t.Errorf("%s is %v after printing help, was %v", cmd.CommandPath(), cmd.Hidden,
				before[cmd.CommandPath()])
		}
	}
}

func leavesThatAreNotMachinery(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, cmd := range leaves(conditionalRoot(t)) {
		if isMachinery(cmd) {
			continue
		}
		if _, ok := whenOf(cmd); !ok {
			continue
		}
		names = append(names, leafName(cmd))
	}
	return names
}

func TestADormantLeafIsLeftOutOfTheListAndCounted(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	c := canonicalOf(t, place.CanonicalCommander)
	out := helpIn(t, c, "")
	for _, name := range []string{"fleet", "vpn", "env", "deploy", "context"} {
		if lists(out, name) {
			t.Errorf("%s is listed on a commander although nothing is approved:\n%s", name, out)
		}
	}
	want := len(leavesThatAreNotMachinery(t))
	line := strconv.Itoa(want) +
		" commands are hidden here; 'caramelo manual --role all' lists every command of every role."
	if !strings.HasSuffix(out, "\n"+line+"\n") {
		t.Errorf("help does not end with %q:\n%s", line, out)
	}
}

func TestGoldenHelpOfACommanderWithNothingApproved(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	checkGolden(t, "help-dormant-commander", helpIn(t, canonicalOf(t, place.CanonicalCommander), ""))
}
