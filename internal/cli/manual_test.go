package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/cpuguy83/go-md2man/v2/md2man"
	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/place"
)

func manualRoot(t *testing.T) *cobra.Command {
	t.Helper()
	var out bytes.Buffer
	root := newRootCmd(&app{stdout: &out, stderr: &out})
	root.InitDefaultHelpCmd()
	return root
}

func visibleCommands(root *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, s := range manualChildren(c) {
			out = append(out, s)
			walk(s)
		}
	}
	walk(root)
	return out
}

func manualHere(t *testing.T, name string) (manual, *manualScope) {
	t.Helper()
	c := canonicalOf(t, name)
	s := &manualScope{role: c.Role, ctx: c, here: true}
	return buildManual(manualRoot(t), s), s
}

func manualOfRole(t *testing.T, role string) (manual, *manualScope) {
	t.Helper()
	s, err := manualScopeOfRole(role)
	if err != nil {
		t.Fatalf("manual --role %s: %v", role, err)
	}
	return buildManual(manualRoot(t), s), s
}

func manualPaths(m manual) []string {
	var out []string
	var walk func(cs []manualCommand)
	walk = func(cs []manualCommand) {
		for _, c := range cs {
			out = append(out, c.Path)
			walk(c.Subcommands)
		}
	}
	walk(m.Commands)
	return out
}

func TestManualCoversEveryCommandInEveryFormat(t *testing.T) {
	for _, role := range manualRoles() {
		t.Run(role, func(t *testing.T) {
			m, _ := manualOfRole(t, role)
			paths := manualPaths(m)
			if len(paths) < 5 {
				t.Fatalf("the manual of a %s lists %d commands; the tree did not register", role, len(paths))
			}
			plain := renderPlain(m)
			md := renderManual(m, false)
			body, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			var back manual
			if err := json.Unmarshal(body, &back); err != nil {
				t.Fatal(err)
			}
			structured := map[string]bool{}
			for _, path := range manualPaths(back) {
				structured[path] = true
			}
			for _, path := range paths {
				if !strings.Contains(plain, "\n"+strings.ToUpper(path)+"\n") {
					t.Errorf("the plain manual of a %s lacks a section for %q", role, path)
				}
				if !strings.Contains(md, "\n## "+path+"\n") {
					t.Errorf("the markdown manual of a %s lacks a heading for %q", role, path)
				}
				if !structured[path] {
					t.Errorf("the JSON manual of a %s lacks %q", role, path)
				}
			}
		})
	}
}

func TestEveryRoleIsPartOfTheManualOfAll(t *testing.T) {
	root := manualRoot(t)
	all := buildManual(root, everyRoleScope())
	tree := map[string]bool{}
	for _, c := range visibleCommands(root) {
		tree[c.CommandPath()] = true
	}
	if len(tree) < 60 {
		t.Fatalf("only %d commands found; the tree did not register", len(tree))
	}
	everywhere := map[string]bool{}
	for _, path := range manualPaths(all) {
		if !tree[path] {
			t.Errorf("--role all lists %q, which is not a command of the tree", path)
		}
		everywhere[path] = true
	}
	for path := range tree {
		if !everywhere[path] {
			t.Errorf("--role all leaves out %q", path)
		}
	}
	for _, name := range place.CanonicalNames() {
		m, _ := manualHere(t, name)
		paths := manualPaths(m)
		if len(paths) >= len(everywhere) {
			t.Errorf("the manual of the canonical %s lists %d of the %d commands there are",
				name, len(paths), len(everywhere))
		}
		for _, path := range paths {
			if !everywhere[path] {
				t.Errorf("the manual of the canonical %s lists %q, which --role all does not", name, path)
			}
		}
	}
}

func TestEveryCommandOfTheTreeSaysWhatItIsFor(t *testing.T) {
	for _, c := range visibleCommands(manualRoot(t)) {
		if strings.TrimSpace(c.Short) == "" {
			t.Errorf("%q has no Short description", c.CommandPath())
		}
	}
}

func TestEveryCommandOfTheManualSaysWhereItHolds(t *testing.T) {
	m, _ := manualOfRole(t, manualRoleAll)
	forms := []struct {
		name string
		out  string
	}{
		{"plain", renderPlain(m)},
		{"markdown", renderManual(m, false)},
	}
	var walk func(cs []manualCommand)
	walk = func(cs []manualCommand) {
		for _, c := range cs {
			if len(c.Subcommands) > 0 {
				if c.Holds != "" {
					t.Errorf("%q is a group and carries a condition of its own: %q", c.Path, c.Holds)
				}
				walk(c.Subcommands)
				continue
			}
			if c.Holds == "" {
				t.Errorf("%q does not say where it holds", c.Path)
				continue
			}
			for _, form := range forms {
				if !strings.Contains(form.out, holdsOnLine(c.Holds)) {
					t.Errorf("the %s section of %q does not print %q",
						form.name, c.Path, holdsOnLine(c.Holds))
				}
			}
		}
	}
	walk(m.Commands)
}

func TestManualCommandFormats(t *testing.T) {
	commanderPlace(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"manual"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("manual: exit %d: %s", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "laptop, commander · ") {
		t.Errorf("the plain manual does not open with the header of this place:\n%s", firstLines(stdout.String(), 3))
	}
	if !strings.Contains(stdout.String(), "\nCARAMELO(1)") || !strings.Contains(stdout.String(), "\nNAME\n") {
		t.Errorf("plain manual does not look like a manual page:\n%s", firstLines(stdout.String(), 8))
	}
	if strings.Contains(stdout.String(), "\n## ") || strings.Contains(stdout.String(), "```") {
		t.Errorf("plain manual contains markdown syntax")
	}

	stdout.Reset()
	if code := Run([]string{"manual", "--markdown"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("manual --markdown: exit %d: %s", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "# caramelo manual\n\nlaptop, commander · ") {
		t.Errorf("markdown manual starts with %q", firstLines(stdout.String(), 3))
	}

	stdout.Reset()
	if code := Run([]string{"manual", "--man"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("manual --man: exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `.TH CARAMELO 1 `) {
		t.Errorf("man output has no .TH header:\n%s", firstLines(stdout.String(), 3))
	}
	if !strings.Contains(stdout.String(), "laptop, commander") {
		t.Errorf("man output does not say where it was rendered:\n%s", firstLines(stdout.String(), 20))
	}

	stdout.Reset()
	if code := Run([]string{"manual", "--role", "hub", "--markdown"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("manual --role hub --markdown: exit %d: %s", code, stderr.String())
	}
	hub := strings.Join(canonicalOf(t, place.CanonicalHub).HeaderParts(), " · ")
	if !strings.HasPrefix(stdout.String(), "# caramelo manual\n\n"+hub+"\n\n") {
		t.Errorf("the markdown manual of a hub does not open with the canonical hub's header:\n%s",
			firstLines(stdout.String(), 4))
	}
	if !strings.Contains(stdout.String(), "This is the manual of a canonical hub, not of this machine") {
		t.Errorf("the markdown manual of a hub does not say it is not of this machine:\n%s",
			firstLines(stdout.String(), 6))
	}

	stdout.Reset()
	if code := Run([]string{"manual", "--markdown", "--man"}, &stdout, &stderr); code != ExitUsage {
		t.Errorf("manual --markdown --man: exit %d, want %d", code, ExitUsage)
	}

	stdout.Reset()
	if code := Run([]string{"manual", "--json"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("manual --json: exit %d: %s", code, stderr.String())
	}
	var m manual
	if err := json.Unmarshal(stdout.Bytes(), &m); err != nil {
		t.Fatalf("manual --json is not JSON: %v", err)
	}
	if len(m.Commands) == 0 || len(m.GlobalFlags) != 4 || m.ExitCodes["usage"] != ExitUsage {
		t.Errorf("manual --json = %d commands, %d global flags, exit codes %v", len(m.Commands), len(m.GlobalFlags), m.ExitCodes)
	}
}

func TestTheManualIsTheManualOfThePlaceItRunsIn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		where  func(t *testing.T)
		header string
		lists  []string
		absent []string
	}{
		{name: "fresh", where: freshPlace, header: "fresh box: no config",
			lists: []string{"caramelo commander init", "caramelo context", "caramelo fleet setup", "caramelo manual"},
			absent: []string{"caramelo deploy", "caramelo env create", "caramelo vpn up", "caramelo fleet list",
				"caramelo member join"}},
		{name: "commander", where: commanderPlace, header: "laptop, commander",
			lists:  []string{"caramelo deploy", "caramelo vpn up", "caramelo fleet list", "caramelo fleet setup"},
			absent: []string{"caramelo hub uninstall", "caramelo member join", "caramelo member leave"}},
		{name: "hub", where: hubPlace, header: "box, hub of fleet home",
			lists:  []string{"caramelo hub status", "caramelo member add", "caramelo env create"},
			absent: []string{"caramelo fleet list", "caramelo vpn up", "caramelo commander init"}},
		{name: "member", where: memberPlace, header: "worker, member of fleet home",
			lists:  []string{"caramelo member leave", "caramelo hub status", "caramelo edge status"},
			absent: []string{"caramelo env create", "caramelo deploy", "caramelo fleet list"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.where(t)
			code, stdout, stderr := run(t, "manual")
			if code != ExitOK {
				t.Fatalf("exit = %d: %s", code, stderr)
			}
			if !strings.HasPrefix(stdout, tc.header) {
				t.Errorf("the manual does not open with %q:\n%s", tc.header, firstLines(stdout, 3))
			}
			for _, path := range tc.lists {
				if !strings.Contains(stdout, "\n"+strings.ToUpper(path)+"\n") {
					t.Errorf("the manual of a %s has no section for %q", tc.name, path)
				}
			}
			for _, path := range tc.absent {
				if strings.Contains(stdout, "\n"+strings.ToUpper(path)+"\n") {
					t.Errorf("the manual of a %s has a section for %q, which does not hold there", tc.name, path)
				}
			}
		})
	}
}

func TestTheManualOfAnotherRoleIsRenderedAgainstACanonicalMachine(t *testing.T) {
	for _, tc := range []struct {
		role   string
		lists  []string
		absent []string
	}{
		{role: place.RoleCommander,
			lists:  []string{"caramelo fleet list", "caramelo vpn up", "caramelo commander init"},
			absent: []string{"caramelo member join", "caramelo member leave", "caramelo hub uninstall"}},
		{role: place.RoleHub,
			lists:  []string{"caramelo hub status", "caramelo member add", "caramelo env create"},
			absent: []string{"caramelo fleet list", "caramelo vpn up", "caramelo commander init"}},
		{role: place.RoleMember,
			lists:  []string{"caramelo member leave", "caramelo hub status", "caramelo edge status"},
			absent: []string{"caramelo env create", "caramelo deploy", "caramelo fleet list"}},
	} {
		t.Run(tc.role, func(t *testing.T) {
			freshPlace(t)
			code, stdout, stderr := run(t, "manual", "--role", tc.role)
			if code != ExitOK {
				t.Fatalf("exit = %d: %s", code, stderr)
			}
			header := strings.Join(headerLines(canonicalOf(t, tc.role).HeaderParts()), "\n") + "\n"
			if !strings.HasPrefix(stdout, header) {
				t.Errorf("manual --role %s does not open with the header of the canonical %s;\nwant %q\ngot\n%s",
					tc.role, tc.role, header, firstLines(stdout, 5))
			}
			note := fmt.Sprintf("This is the manual of a canonical %s, not of this machine", tc.role)
			if !strings.Contains(stdout, note) {
				t.Errorf("nothing says %q:\n%s", note, firstLines(stdout, 6))
			}
			if strings.Contains(stdout, "hidden here") {
				t.Errorf("the manual of a %s counts what it hides:\n%s", tc.role, firstLines(stdout, 6))
			}
			for _, path := range tc.lists {
				if !strings.Contains(stdout, "\n"+strings.ToUpper(path)+"\n") {
					t.Errorf("the manual of a %s has no section for %q", tc.role, path)
				}
			}
			for _, path := range tc.absent {
				if strings.Contains(stdout, "\n"+strings.ToUpper(path)+"\n") {
					t.Errorf("the manual of a %s has a section for %q, which does not hold there", tc.role, path)
				}
			}
		})
	}
}

func TestTheManualOfEveryRoleHidesNothing(t *testing.T) {
	memberPlace(t)
	code, stdout, stderr := run(t, "manual", "--role", "all")
	if code != ExitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if !strings.HasPrefix(stdout, manualEveryRole) {
		t.Errorf("manual --role all does not open by saying so:\n%s", firstLines(stdout, 3))
	}
	if strings.Contains(stdout, "hidden here") {
		t.Errorf("manual --role all counts hidden commands:\n%s", firstLines(stdout, 5))
	}
	for _, path := range []string{"caramelo vpn up", "caramelo fleet list", "caramelo env create",
		"caramelo member leave", "caramelo commander init"} {
		if !strings.Contains(stdout, "\n"+strings.ToUpper(path)+"\n") {
			t.Errorf("manual --role all has no section for %q", path)
		}
	}
}

func TestAnUnknownRoleIsAUsageError(t *testing.T) {
	freshPlace(t)
	code, stdout, stderr := run(t, "manual", "--role", "laptop")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	if !strings.Contains(stderr, `--role is one of commander, hub, member, all, not "laptop"`) {
		t.Errorf("stderr = %q, want it to name the roles there are", stderr)
	}
}

func TestTheManualSaysHowManyCommandsItLeftOut(t *testing.T) {
	for _, name := range place.CanonicalNames() {
		t.Run(name, func(t *testing.T) {
			c := canonicalOf(t, name)
			m, s := manualHere(t, name)
			want := len(hiddenByThePlace(t, c))
			if want == 0 {
				t.Fatalf("nothing is hidden on the canonical %s; the count cannot be checked", name)
			}
			if s.hidden != want {
				t.Errorf("the manual left out %d commands, the place hides %d", s.hidden, want)
			}
			if m.note != hiddenHere(want) {
				t.Errorf("the note = %q, want %q", m.note, hiddenHere(want))
			}
			line := strconv.Itoa(want) + " commands are hidden here"
			if !strings.Contains(renderPlain(m), line) {
				t.Errorf("the plain manual does not say %q", line)
			}
			md := renderManual(m, false)
			if !strings.Contains(md, hiddenHere(want)) {
				t.Errorf("the markdown manual does not say %q", hiddenHere(want))
			}
			roff := string(md2man.Render([]byte(renderManual(m, true))))
			if !strings.Contains(roff, hiddenHere(want)) {
				t.Errorf("the roff manual does not say %q", hiddenHere(want))
			}
		})
	}
}

func TestTheManualOfAPlaceThatCannotBeReadIsEveryRole(t *testing.T) {
	var out bytes.Buffer
	a := &app{stdout: &out, stderr: &out}
	a.placeOnce.Do(func() { a.placeErr = errors.New("locate the commander config: no home directory") })
	s, err := a.manualScope(&cobra.Command{}, "")
	if err != nil {
		t.Fatalf("a place that cannot be read: %v", err)
	}
	if !s.every || s.role != manualRoleAll {
		t.Errorf("scope = %+v, want every command of every role", s)
	}
}

func TestManualJSONSaysTheRoleAndThePlace(t *testing.T) {
	hubPlace(t)
	code, stdout, stderr := run(t, "manual", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	var m manual
	if err := json.Unmarshal([]byte(stdout), &m); err != nil {
		t.Fatalf("manual --json is not JSON: %v", err)
	}
	if m.Role != place.RoleHub {
		t.Errorf("role = %q, want %q", m.Role, place.RoleHub)
	}
	if m.Context == nil {
		t.Fatal("the JSON manual carries no context")
	}
	if m.Context.Role != place.RoleHub || m.Context.Name != "box" {
		t.Errorf("context = %q on %q, want a hub named box", m.Context.Role, m.Context.Name)
	}
	if holds := manualHolds(m, "caramelo hub status"); holds != "anything but a commander" {
		t.Errorf("hub status holds on %q", holds)
	}

	code, stdout, stderr = run(t, "manual", "--role", "all", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	var every manual
	if err := json.Unmarshal([]byte(stdout), &every); err != nil {
		t.Fatalf("manual --role all --json is not JSON: %v", err)
	}
	if every.Role != manualRoleAll || every.Context != nil {
		t.Errorf("role = %q with context %v, want %q and no place", every.Role, every.Context, manualRoleAll)
	}

	for _, name := range place.CanonicalNames() {
		t.Run(name, func(t *testing.T) {
			c := canonicalOf(t, name)
			here, _ := manualHere(t, name)
			body, err := json.Marshal(here)
			if err != nil {
				t.Fatal(err)
			}
			var back manual
			if err := json.Unmarshal(body, &back); err != nil {
				t.Fatal(err)
			}
			if back.Role != c.Role {
				t.Errorf("role = %q, want %q", back.Role, c.Role)
			}
			if back.Context == nil {
				t.Fatalf("the JSON manual of the canonical %s carries no context", name)
			}
			if back.Context.Role != back.Role {
				t.Errorf("role = %q with a context of a %q; the manual and its place disagree",
					back.Role, back.Context.Role)
			}
			if back.Context.Name != c.Name {
				t.Errorf("context names %q, want %q", back.Context.Name, c.Name)
			}
		})
	}
}

func manualHolds(m manual, path string) string {
	var found string
	var walk func(cs []manualCommand)
	walk = func(cs []manualCommand) {
		for _, c := range cs {
			if c.Path == path {
				found = c.Holds
			}
			walk(c.Subcommands)
		}
	}
	walk(m.Commands)
	return found
}

func manualOutline(t *testing.T, out string) string {
	t.Helper()
	opening, rest, ok := strings.Cut(out, "CARAMELO(1)")
	if !ok {
		t.Fatalf("the manual has no title line:\n%s", firstLines(out, 5))
	}
	_, list, ok := strings.Cut(rest, "\nCOMMANDS\n")
	if !ok {
		t.Fatal("the manual has no command index")
	}
	var b strings.Builder
	b.WriteString(opening)
	b.WriteString("COMMANDS\n")
	for _, line := range strings.Split(list, "\n") {
		if strings.TrimSpace(line) == "" {
			break
		}
		fmt.Fprintln(&b, line)
	}
	return b.String()
}

func TestGoldenManualOfEveryPlace(t *testing.T) {
	for _, name := range place.CanonicalNames() {
		t.Run(name, func(t *testing.T) {
			m, _ := manualHere(t, name)
			checkGolden(t, "manual-"+name, manualOutline(t, renderPlain(m)))
		})
	}
	t.Run("every-role", func(t *testing.T) {
		m, _ := manualOfRole(t, manualRoleAll)
		checkGolden(t, "manual-every-role", manualOutline(t, renderPlain(m)))
	})
}

var exampleCommand = regexp.MustCompile(`caramelo\s+([^#|)"']*)`)

func TestExamplesNameRealCommandsAndFlags(t *testing.T) {
	root := manualRoot(t)
	for path := range examples {
		if _, _, err := root.Find(strings.Fields(path)[1:]); err != nil {
			t.Errorf("examples has an entry for %q, which is not a command", path)
		}
	}
	var seen int
	for _, c := range visibleCommands(root) {
		if c.Example == "" {
			if len(c.Commands()) == 0 {
				t.Errorf("%q has no example", c.CommandPath())
			}
			continue
		}
		for _, line := range strings.Split(c.Example, "\n") {
			ms := exampleCommand.FindAllStringSubmatch(line, -1)
			if len(ms) == 0 {
				continue
			}
			seen++
			args := strings.Fields(ms[len(ms)-1][1])
			target, rest, err := root.Find(args)
			if err != nil {
				t.Errorf("%q example %q: %v", c.CommandPath(), line, err)
				continue
			}
			for _, tok := range rest {
				if tok == "--" {
					break
				}
				if !strings.HasPrefix(tok, "-") || tok == "-" {
					continue
				}
				name := strings.TrimLeft(strings.SplitN(tok, "=", 2)[0], "-")
				var found bool
				if strings.HasPrefix(tok, "--") {
					found = target.Flags().Lookup(name) != nil || target.InheritedFlags().Lookup(name) != nil
				} else {
					found = target.Flags().ShorthandLookup(name) != nil || target.InheritedFlags().ShorthandLookup(name) != nil
				}
				if !found {
					t.Errorf("%q example %q uses %s, which %q does not have", c.CommandPath(), strings.TrimSpace(line), tok, target.CommandPath())
				}
			}
		}
	}
	if seen < 100 {
		t.Errorf("only %d example lines checked", seen)
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func sortedPaths(paths []string) []string {
	out := append([]string(nil), paths...)
	slices.Sort(out)
	return out
}

func pathAndItsGroups(cmd *cobra.Command) []string {
	var out []string
	for c := cmd; c != nil && c.HasParent(); c = c.Parent() {
		out = append(out, c.CommandPath())
	}
	return out
}

func documentedHere(t *testing.T, c place.Context) []string {
	t.Helper()
	ap, judged := approvalFor(c)
	if !judged {
		t.Fatalf("the canonical %s is judged by no list", c.Role)
	}
	seen := map[string]bool{}
	var out []string
	for _, cmd := range leaves(manualRoot(t)) {
		if isMachinery(cmd) {
			continue
		}
		w, held := whenOf(cmd)
		if !held || !w.ok(c) || !ap.covers(leafName(cmd)) {
			continue
		}
		for _, path := range pathAndItsGroups(cmd) {
			if seen[path] {
				continue
			}
			seen[path] = true
			out = append(out, path)
		}
	}
	return out
}

func TestTheManualOfThisMachineLeavesOutADormantCommand(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	m, s := manualHere(t, place.CanonicalCommander)
	c := canonicalOf(t, place.CanonicalCommander)
	got, documented := sortedPaths(manualPaths(m)), sortedPaths(documentedHere(t, c))
	if !slices.Equal(got, documented) {
		t.Errorf("the manual of a commander documents %v, want the approved commands that hold there "+
			"and the groups they sit under, %v", got, documented)
	}
	want := hiddenHereCount(t, c)
	if s.hidden != want {
		t.Errorf("the manual left out %d commands, want the %d that hold here and are on no list",
			s.hidden, want)
	}
	if m.note != hiddenHere(want) {
		t.Errorf("the note = %q, want %q", m.note, hiddenHere(want))
	}
}

func TestTheManualOfARoleIsScopedByThatRolesList(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	for _, role := range manualRoles() {
		if role == manualRoleAll {
			continue
		}
		t.Run(role, func(t *testing.T) {
			m, s := manualOfRole(t, role)
			got, documented := sortedPaths(manualPaths(m)), sortedPaths(documentedHere(t, s.ctx))
			if !slices.Equal(got, documented) {
				t.Errorf("the manual of a canonical %s documents %v, want %v", role, got, documented)
			}
			if want := hiddenHereCount(t, s.ctx); s.hidden != want {
				t.Errorf("the manual of a canonical %s left out %d commands, want the %d that hold there "+
					"and are on no list", role, s.hidden, want)
			}
		})
	}
}

func TestRoleAllStillListsADormantCommand(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	root := manualRoot(t)
	all := buildManual(root, everyRoleScope())
	listed := map[string]bool{}
	for _, path := range manualPaths(all) {
		listed[path] = true
	}
	for _, path := range []string{"caramelo fleet list", "caramelo vpn up", "caramelo env create",
		"caramelo commander init", "caramelo context"} {
		if !listed[path] {
			t.Errorf("--role all leaves out %q, which is dormant and not gone", path)
		}
	}
	if got, want := len(listed), len(visibleCommands(root)); got != want {
		t.Errorf("--role all lists %d of the %d commands there are", got, want)
	}
}

func TestTheManualNamesTheExperimentalSwitch(t *testing.T) {
	m, _ := manualOfRole(t, manualRoleAll)
	if !strings.Contains(renderPlain(m), "\n    "+experimentalEnv+"\n") {
		t.Errorf("the plain manual's environment table does not carry %s", experimentalEnv)
	}
	if !strings.Contains(renderManual(m, false), "| `"+experimentalEnv+"` |") {
		t.Errorf("the markdown manual's environment table does not carry %s", experimentalEnv)
	}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back manual
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range back.Environment {
		if e.Name == experimentalEnv {
			found = true
		}
	}
	if !found {
		t.Errorf("the JSON manual's environment does not carry %s", experimentalEnv)
	}
	if !strings.Contains(renderPlain(m), "dormant") {
		t.Error("nothing in the manual says what a dormant command is")
	}
}

func TestGoldenManualOfACommanderWithOnlyTheSetupApproved(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	m, _ := manualHere(t, place.CanonicalCommander)
	checkGolden(t, "manual-dormant-commander", manualOutline(t, renderPlain(m)))
}

func TestGoldenManualOfAHubWithOnlyTheSetupApproved(t *testing.T) {
	t.Setenv(experimentalEnv, "")
	m, _ := manualHere(t, place.CanonicalHub)
	checkGolden(t, "manual-dormant-hub", manualOutline(t, renderPlain(m)))
}
