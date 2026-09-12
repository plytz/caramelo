package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/cpuguy83/go-md2man/v2/md2man"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func init() {
	register(func(a *app) *cobra.Command { return a.manualCmd() })
}

type manualFlag struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Default   string `json:"default,omitempty"`
	Usage     string `json:"usage"`
}

type manualCommand struct {
	Path        string          `json:"path"`
	Usage       string          `json:"usage"`
	Short       string          `json:"short"`
	Long        string          `json:"long,omitempty"`
	Example     string          `json:"example,omitempty"`
	Flags       []manualFlag    `json:"flags,omitempty"`
	Subcommands []manualCommand `json:"subcommands,omitempty"`
}

type manual struct {
	Name        string          `json:"name"`
	Short       string          `json:"short"`
	Description string          `json:"description"`
	Agents      []string        `json:"agents"`
	ExitCodes   map[string]int  `json:"exit_codes"`
	Environment []manualFlag    `json:"environment"`
	GlobalFlags []manualFlag    `json:"global_flags"`
	Commands    []manualCommand `json:"commands"`
}

var manualAgentNotes = []string{
	"Every command accepts --json. A command that returns a result then prints exactly one JSON document on standard output and nothing else; events and logs print one JSON object per line; run, test and env exec give standard output to the command they run.",
	"Progress and diagnostics go to standard error. With --progress json each line on standard error is one JSON event with the same shape as `caramelo events --json`.",
	"Exit codes are stable: 0 is success, 1 is a failure the command reports, 2 is a usage error (unknown command, bad flag, missing argument).",
	"No command needs a terminal. Anything a person is asked interactively can be answered with a flag, and when standard input is not a terminal the command fails at once naming that flag instead of waiting.",
	"The machine a command talks to is picked by --machine (CARAMELO_MACHINE is its default), then a local caramelod socket if there is one, then the client config's default_machine.",
	"The app and the environment default to the git checkout the command runs in; --app and --env (or CARAMELO_APP and CARAMELO_ENV) name them explicitly.",
	"Names in the manual are placeholders: feat-x is an environment, shop is an app, box is a machine.",
}

var manualEnvironment = []manualFlag{
	{Name: "CARAMELO_MACHINE", Type: "string", Usage: "the machine to talk to, as --machine"},
	{Name: "CARAMELO_APP", Type: "string", Usage: "the app, as --app on the commands that take one"},
	{Name: "CARAMELO_ENV", Type: "string", Usage: "the environment, as --env on the commands that take one"},
	{Name: "CARAMELO_SSH", Type: "string", Usage: "the ssh program used to reach a user@host machine (default: ssh)"},
	{Name: "CARAMELO_SSH_OPTS", Type: "string", Usage: "extra arguments for that ssh, split like shell words"},
	{Name: "CARAMELO_DEBUG", Type: "string", Usage: "when set, say on standard error which transport was chosen and why"},
	{Name: "XDG_CONFIG_HOME", Type: "path", Usage: "absolute path under which the client config and keys live, in caramelo/ (default: the platform's user config dir)"},
	{Name: "XDG_CACHE_HOME", Type: "path", Usage: "absolute path under which ssh control sockets live, in caramelo/ (default: the platform's user cache dir)"},
	{Name: "XDG_RUNTIME_DIR", Type: "path", Usage: "where the transparent-mode service's control socket lives, in caramelo/ (default: the cache dir)"},
}

func (a *app) manualCmd() *cobra.Command {
	var markdown, man bool
	cmd := &cobra.Command{
		Use:   "manual",
		Short: "The complete manual: every command, every flag, with examples",
		Long: `manual renders the whole command tree as one document: what each command
does, its flags with their defaults, and examples. The text is generated from
the same definitions the commands run on, so it cannot drift from the binary.

By default the manual is plain text laid out like a manual page, for reading
in a terminal (pipe it to less). --markdown prints the Markdown source, the
form an agent reads and the MANUAL.md attached to every release is generated
from. --man prints a roff manual page: pipe it to man, or install it as
caramelo.1. With --json the manual is a structured tree of commands and
flags instead.`,
		Example: `  caramelo manual | less
  caramelo manual --markdown > MANUAL.md
  caramelo manual --man | man -l -
  caramelo manual --man > /usr/local/share/man/man1/caramelo.1
  caramelo manual --json | jq '.commands[] | select(.path == "caramelo deploy")'`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if markdown && man {
				return &usageError{fmt.Errorf("--markdown and --man are opposites; pick one")}
			}
			m := buildManual(cmd.Root())
			return a.printer().Result(m, func(w io.Writer) error {
				var err error
				switch {
				case man:
					_, err = w.Write(md2man.Render([]byte(renderManual(m, true))))
				case markdown:
					_, err = io.WriteString(w, renderManual(m, false))
				default:
					_, err = io.WriteString(w, renderPlain(m))
				}
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&markdown, "markdown", false, "print the Markdown source instead of plain text")
	cmd.Flags().BoolVar(&man, "man", false, "print a roff manual page instead of plain text")
	return cmd
}

func buildManual(root *cobra.Command) manual {
	m := manual{
		Name:        root.Name(),
		Short:       root.Short,
		Description: strings.TrimSpace(root.Long),
		Agents:      manualAgentNotes,
		ExitCodes:   map[string]int{"ok": ExitOK, "error": ExitError, "usage": ExitUsage},
		Environment: manualEnvironment,
		GlobalFlags: manualFlags(root.PersistentFlags()),
	}
	for _, c := range manualChildren(root) {
		m.Commands = append(m.Commands, buildManualCommand(c))
	}
	return m
}

func manualChildren(cmd *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, c := range cmd.Commands() {
		if c.Hidden || c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func buildManualCommand(cmd *cobra.Command) manualCommand {
	mc := manualCommand{
		Path:    cmd.CommandPath(),
		Usage:   cmd.UseLine(),
		Short:   cmd.Short,
		Long:    strings.TrimSpace(cmd.Long),
		Example: strings.TrimSpace(dedent(strings.TrimRight(cmd.Example, "\n"))),
		Flags:   manualFlags(cmd.NonInheritedFlags()),
	}
	for _, c := range manualChildren(cmd) {
		mc.Subcommands = append(mc.Subcommands, buildManualCommand(c))
	}
	return mc
}

func manualFlags(fs *pflag.FlagSet) []manualFlag {
	var out []manualFlag
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		def := f.DefValue
		if def == "false" || def == "[]" || def == "0s" {
			def = ""
		}
		out = append(out, manualFlag{Name: f.Name, Shorthand: f.Shorthand, Type: f.Value.Type(), Default: def, Usage: f.Usage})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func renderManual(m manual, forMan bool) string {
	var b strings.Builder
	if forMan {
		fmt.Fprintf(&b, "# %s 1 \"\" \"%s\" \"%s manual\"\n\n", strings.ToUpper(m.Name), m.Name, m.Name)
		fmt.Fprintf(&b, "# NAME\n\n%s - %s\n\n", m.Name, m.Short)
		fmt.Fprintf(&b, "# SYNOPSIS\n\n**%s** [command] [flags]\n\n", m.Name)
		fmt.Fprintf(&b, "# DESCRIPTION\n\n%s\n\n", m.Description)
	} else {
		fmt.Fprintf(&b, "# %s manual\n\n%s\n\n", m.Name, m.Description)
	}

	fmt.Fprintf(&b, "%s\n\n", manualHeading(forMan, 1, "For agents"))
	for _, n := range m.Agents {
		fmt.Fprintf(&b, "- %s\n", n)
	}
	fmt.Fprintf(&b, "\n%s\n\n", manualHeading(forMan, 1, "Exit codes"))
	fmt.Fprintf(&b, "| Code | Meaning |\n|---|---|\n| %d | success |\n| %d | the command failed and said why on standard error |\n| %d | usage error: unknown command, bad flag or missing argument |\n\n",
		m.ExitCodes["ok"], m.ExitCodes["error"], m.ExitCodes["usage"])

	fmt.Fprintf(&b, "%s\n\n", manualHeading(forMan, 1, "Environment"))
	fmt.Fprintf(&b, "| Variable | Description |\n|---|---|\n")
	for _, e := range m.Environment {
		fmt.Fprintf(&b, "| `%s` | %s |\n", e.Name, manualCell(e.Usage))
	}
	fmt.Fprintf(&b, "\n%s\n\n", manualHeading(forMan, 1, "Global flags"))
	fmt.Fprintf(&b, "Every command accepts these.\n\n")
	writeFlagTable(&b, m.GlobalFlags)

	fmt.Fprintf(&b, "\n%s\n\n", manualHeading(forMan, 1, "Commands"))
	for _, c := range m.Commands {
		writeIndex(&b, c, 0)
	}
	b.WriteString("\n")
	for _, c := range m.Commands {
		writeCommand(&b, c, forMan)
	}
	return b.String()
}

func writeIndex(b *strings.Builder, c manualCommand, depth int) {
	fmt.Fprintf(b, "%s- `%s`: %s\n", strings.Repeat("  ", depth), c.Path, c.Short)
	for _, s := range c.Subcommands {
		writeIndex(b, s, depth+1)
	}
}

func writeCommand(b *strings.Builder, c manualCommand, forMan bool) {
	fmt.Fprintf(b, "%s\n\n", manualHeading(forMan, 1, c.Path))
	fmt.Fprintf(b, "%s\n\n", c.Short)
	fmt.Fprintf(b, "```\n%s\n```\n\n", c.Usage)
	if c.Long != "" {
		fmt.Fprintf(b, "%s\n\n", c.Long)
	}
	if c.Example != "" {
		fmt.Fprintf(b, "%s\n\n```sh\n%s\n```\n\n", manualHeading(forMan, 2, "Examples"), c.Example)
	}
	if len(c.Flags) > 0 {
		fmt.Fprintf(b, "%s\n\n", manualHeading(forMan, 2, "Flags"))
		writeFlagTable(b, c.Flags)
		b.WriteString("\n")
	}
	if len(c.Subcommands) > 0 {
		fmt.Fprintf(b, "%s\n\n", manualHeading(forMan, 2, "Subcommands"))
		for _, s := range c.Subcommands {
			fmt.Fprintf(b, "- `%s`: %s\n", s.Path, s.Short)
		}
		b.WriteString("\n")
	}
	for _, s := range c.Subcommands {
		writeCommand(b, s, forMan)
	}
}

func writeFlagTable(b *strings.Builder, flags []manualFlag) {
	fmt.Fprintf(b, "| Flag | Type | Default | Description |\n|---|---|---|---|\n")
	for _, f := range flags {
		name := "`--" + f.Name + "`"
		if f.Shorthand != "" {
			name = "`-" + f.Shorthand + "`, " + name
		}
		def := ""
		if f.Default != "" {
			def = "`" + f.Default + "`"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", name, f.Type, def, manualCell(f.Usage))
	}
}

func manualHeading(forMan bool, level int, text string) string {
	if forMan {
		if level == 1 {
			return "# " + strings.ToUpper(text)
		}
		return "## " + text
	}
	return strings.Repeat("#", level+1) + " " + text
}

func manualCell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(s, "\n", " ")
}

func dedent(s string) string {
	lines := strings.Split(s, "\n")
	indent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, " \t"))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	if indent <= 0 {
		return s
	}
	for i, l := range lines {
		if len(l) >= indent {
			lines[i] = l[indent:]
		}
	}
	return strings.Join(lines, "\n")
}

func renderPlain(m manual) string {
	var b strings.Builder
	title := strings.ToUpper(m.Name) + "(1)"
	fmt.Fprintf(&b, "%s%s%s\n\n", title, strings.Repeat(" ", max(1, 78-2*len(title)-len(m.Name+" manual"))), m.Name+" manual")
	fmt.Fprintf(&b, "NAME\n    %s - %s\n\n", m.Name, m.Short)
	fmt.Fprintf(&b, "SYNOPSIS\n    %s [command] [flags]\n\n", m.Name)
	fmt.Fprintf(&b, "DESCRIPTION\n%s\n", indent(m.Description, 4))
	b.WriteString("FOR AGENTS\n")
	for _, n := range m.Agents {
		b.WriteString(indent(hang(wrap(n, 72), "- ", "  "), 4))
	}
	b.WriteString("\nEXIT CODES\n")
	fmt.Fprintf(&b, "    %d    success\n    %d    the command failed and said why on standard error\n    %d    usage error: unknown command, bad flag or missing argument\n\n",
		m.ExitCodes["ok"], m.ExitCodes["error"], m.ExitCodes["usage"])
	b.WriteString("ENVIRONMENT\n")
	for _, e := range m.Environment {
		fmt.Fprintf(&b, "    %s\n%s", e.Name, indent(wrap(e.Usage, 70), 8))
	}
	b.WriteString("\nGLOBAL FLAGS\n    Every command accepts these.\n\n")
	writePlainFlags(&b, m.GlobalFlags, 4)
	b.WriteString("\nCOMMANDS\n")
	for _, c := range m.Commands {
		writePlainIndex(&b, c, 4)
	}
	b.WriteString("\n")
	for _, c := range m.Commands {
		writePlainCommand(&b, c)
	}
	return b.String()
}

func writePlainIndex(b *strings.Builder, c manualCommand, depth int) {
	fmt.Fprintf(b, "%s%-*s%s\n", strings.Repeat(" ", depth), max(1, 34-depth), c.Path, c.Short)
	for _, s := range c.Subcommands {
		writePlainIndex(b, s, depth+2)
	}
}

func writePlainCommand(b *strings.Builder, c manualCommand) {
	fmt.Fprintf(b, "%s\n", strings.ToUpper(c.Path))
	fmt.Fprintf(b, "    %s\n\n", c.Short)
	fmt.Fprintf(b, "    Usage: %s\n\n", c.Usage)
	if c.Long != "" {
		fmt.Fprintf(b, "%s\n", indent(c.Long, 4))
	}
	if c.Example != "" {
		fmt.Fprintf(b, "    Examples:\n%s\n", indent(c.Example, 8))
	}
	if len(c.Flags) > 0 {
		b.WriteString("    Flags:\n")
		writePlainFlags(b, c.Flags, 8)
		b.WriteString("\n")
	}
	if len(c.Subcommands) > 0 {
		b.WriteString("    Subcommands:\n")
		for _, s := range c.Subcommands {
			fmt.Fprintf(b, "        %-30s%s\n", s.Path, s.Short)
		}
		b.WriteString("\n")
	}
	for _, s := range c.Subcommands {
		writePlainCommand(b, s)
	}
}

func writePlainFlags(b *strings.Builder, flags []manualFlag, depth int) {
	pad := strings.Repeat(" ", depth)
	for _, f := range flags {
		head := "--" + f.Name
		if f.Shorthand != "" {
			head = "-" + f.Shorthand + ", " + head
		}
		if f.Type != "bool" {
			head += " " + f.Type
		}
		if f.Default != "" {
			head += " (default " + f.Default + ")"
		}
		fmt.Fprintf(b, "%s%s\n%s", pad, head, indent(wrap(f.Usage, 78-depth-4), depth+4))
	}
}

func indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if strings.TrimSpace(l) == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(pad + l + "\n")
	}
	return b.String()
}

func wrap(s string, width int) string {
	words := strings.Fields(s)
	var b strings.Builder
	line := 0
	for i, w := range words {
		if line > 0 && line+1+len(w) > width {
			b.WriteString("\n")
			line = 0
		} else if i > 0 {
			b.WriteString(" ")
			line++
		}
		b.WriteString(w)
		line += len(w)
	}
	return b.String()
}

func hang(s, first, rest string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if i == 0 {
			lines[i] = first + l
		} else {
			lines[i] = rest + l
		}
	}
	return strings.Join(lines, "\n")
}
