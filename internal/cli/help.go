package cli

import (
	"context"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/place"
)

const headerWidth = 88

const headerIndent = "  "

const headerSeparator = " · "

const headerMaxLines = 3

func (a *app) installHelp(root *cobra.Command) {
	help, usage := root.HelpFunc(), root.UsageFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		c, ok := a.placeToScopeBy(cmd)
		if !ok {
			help(cmd, args)
			return
		}
		scope := hideWhatDoesNotHold(cmd, c)
		defer scope.restore()

		w := cmd.OutOrStdout()
		if cmd.HasSubCommands() {
			writeWhereYouAre(w, c)
		}
		help(cmd, args)
		if cmd.HasSubCommands() && scope.hidden > 0 {
			fmt.Fprintf(w, "\n%s\n", hiddenHere(scope.hidden))
		}
	})
	root.SetUsageFunc(func(cmd *cobra.Command) error {
		c, ok := a.placeToScopeBy(cmd)
		if !ok {
			return usage(cmd)
		}
		scope := hideWhatDoesNotHold(cmd, c)
		defer scope.restore()
		return usage(cmd)
	})
}

func (a *app) placeToScopeBy(cmd *cobra.Command) (place.Context, bool) {
	a.readConfigDirFlag(cmd)
	c, err := a.place(contextOfHelp(cmd))
	if err != nil {
		return place.Context{}, false
	}
	return c, true
}

func contextOfHelp(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

type helpScope struct {
	hidden int
	undo   []func()
}

func hideWhatDoesNotHold(cmd *cobra.Command, c place.Context) *helpScope {
	s := &helpScope{}
	for _, sub := range cmd.Commands() {
		s.keep(sub, c)
	}
	return s
}

func (s *helpScope) keep(cmd *cobra.Command, c place.Context) bool {
	if cmd.Hidden {
		return false
	}
	if !cmd.HasSubCommands() {
		if w, ok := whenOf(cmd); ok && !w.ok(c) {
			s.hide(cmd)
			s.hidden++
			return false
		}
		return true
	}
	shown := false
	for _, sub := range cmd.Commands() {
		if s.keep(sub, c) {
			shown = true
		}
	}
	if !shown {
		s.hide(cmd)
	}
	return shown
}

func (s *helpScope) hide(cmd *cobra.Command) {
	cmd.Hidden = true
	s.undo = append(s.undo, func() { cmd.Hidden = false })
}

func (s *helpScope) restore() {
	for _, undo := range s.undo {
		undo()
	}
	s.undo = nil
}

func writeWhereYouAre(w io.Writer, c place.Context) {
	for _, line := range headerLines(c.HeaderParts()) {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w)
}

func headerLines(parts []string) []string {
	var lines []string
	line := ""
	for _, part := range parts {
		switch {
		case line == "":
			line = part
		case len(lines) == headerMaxLines-1,
			utf8.RuneCountInString(line+headerSeparator+part) <= headerWidth:
			line += headerSeparator + part
		default:
			lines = append(lines, line+" ·")
			line = headerIndent + part
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

func hiddenHere(n int) string {
	commands := fmt.Sprintf("%d commands are", n)
	if n == 1 {
		commands = "1 command is"
	}
	return fmt.Sprintf("%s hidden here; 'caramelo manual --role all' lists every command of every role.",
		commands)
}
