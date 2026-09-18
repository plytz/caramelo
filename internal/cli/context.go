package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/cli/ui"
	"github.com/plytz/caramelo/internal/place"
	"github.com/plytz/caramelo/internal/remote"
)

func init() {
	register(func(a *app) *cobra.Command { return a.contextCmd() })
}

func (a *app) contextCmd() *cobra.Command {
	cmd := localCmd(&cobra.Command{
		Use:   "context",
		Short: "Say where this CLI is running and what a command typed here would act on",
		Long: `context is the answer to "where am I": the name of this machine and its role,
the config file that says so, what that place holds, the fleet a command typed
here would talk to and why, and the app and environment the working directory
names.

It reads this machine only: one or two files, one socket and one 'git rev-parse',
and it never dials a fleet. Run it on a box that puzzles you before running
anything else; --json is the same answer for an agent.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.place(cmd.Context())
			if err != nil {
				return err
			}
			return a.printer().Result(c, func(w io.Writer) error {
				return contextView(c).Write(w)
			})
		},
	})
	return available(cmd, always)
}

func (a *app) place(ctx context.Context) (place.Context, error) {
	a.placeOnce.Do(func() {
		a.placeHere, a.placeErr = place.Detect(ctx, place.Options{
			ConfigDir:    a.configDir,
			Machine:      a.machine,
			Fleet:        a.fleet,
			Git:          runGit,
			SocketExists: socketExists,
		})
	})
	return a.placeHere, a.placeErr
}

func (a *app) readConfigDirFlag(cmd *cobra.Command) {
	f := cmd.Flags().Lookup("config-dir")
	if f == nil {
		return
	}
	a.configDir = strings.TrimSpace(f.Value.String())
}

func contextView(c place.Context) *ui.View {
	f := ui.NewFields(c.Header())
	switch {
	case c.IsServer():
		serverFields(f, c)
	case c.IsCommander():
		commanderFields(f, c)
	default:
		f.Add("looked for", "%s", c.ServerConfigFile())
		f.Add("and for", "%s", c.CommanderConfig)
	}
	workFields(f, c.Work)
	f.Add("talks to", "%s", talksToLine(c.TalksTo))
	if c.Problem != "" {
		f.Note("problem: %s", c.Problem)
	}
	v := ui.NewView().Fields(f)
	if c.Commander != nil && len(c.Commander.Fleets) > 0 {
		v = v.Section(fleetsTable(c.Commander.Fleets))
	}
	return v
}

func serverFields(f *ui.Fields, c place.Context) {
	s := c.Server
	f.Add("config", "%s", c.ConfigFile)
	if c.Commander != nil {
		f.Add("commander", "%s", c.CommanderConfig)
	}
	f.Add("user", "%s:%s", s.User, s.Group)
	if s.Fleet != "" {
		f.Add("fleet", "%s", s.Fleet)
	}
	if s.Hub != nil {
		f.Add("hub", "%s", memberHubLine(s.Hub))
	}
	f.Add("state", "%s", s.Paths.State)
	f.Add("data", "%s", s.Paths.Data)
	f.Add("run", "%s", s.Paths.Run)
	f.Add("apps", "%s", s.Paths.Apps)
	f.Add("edge dir", "%s", s.Paths.Edge)
	f.Add("vpn dir", "%s", s.Paths.VPN)
	f.Add("secrets", "%s", s.Paths.Secrets)
	f.Add("caramelod", "%s", socketLine(s.Services.Daemon))
	f.Add("edge", "%s", edgeLine(s.Services.Edge))
	f.Add("vpn listen", "%s", strOrDash(s.Services.VPNListen))
	f.Add("api listen", "%s", strOrDash(s.Services.APIListen))
}

func memberHubLine(h *place.MemberHub) string {
	parts := []string{h.Label()}
	if h.Address != "" {
		parts = append(parts, h.Address+" inside the tunnel")
	}
	if h.PublicKey != "" {
		parts = append(parts, "key "+remote.ElideKey(h.PublicKey))
	}
	return strings.Join(parts, ", ")
}

func socketLine(s place.Socket) string {
	if s.Present {
		return "socket " + s.Path
	}
	return "no socket at " + s.Path
}

func edgeLine(e place.Edge) string {
	if !e.Enabled {
		return "off"
	}
	return "on, " + socketLine(e.Socket)
}

func commanderFields(f *ui.Fields, c place.Context) {
	cm := c.Commander
	f.Add("config", "%s", c.ConfigFile)
	f.Add("identity", "%s", identityLine(cm))
	f.Add("records", "%s", cm.Records)
	if cm.CacheDir != "" {
		f.Add("cache", "%s", cm.CacheDir)
	}
}

func identityLine(cm *place.Commander) string {
	if cm.Identified {
		return cm.IdentityKey
	}
	return "none yet at " + cm.IdentityKey
}

func workFields(f *ui.Fields, w place.Work) {
	f.Add("directory", "%s", w.Dir)
	f.Add("app", "%s", fromLine(w.App, w.AppFrom, "no app checkout here"))
	f.Add("env", "%s", fromLine(w.Env, w.EnvFrom, "no environment worktree here"))
}

func fromLine(value, from, none string) string {
	if value == "" {
		return none
	}
	if from == "" {
		return value
	}
	return fmt.Sprintf("%s (%s)", value, from)
}

func talksToLine(t place.TalksTo) string {
	switch t.Kind {
	case place.TalksSocket:
		return fmt.Sprintf("the caramelod on this machine, %s (%s)", t.Socket, t.Why)
	case place.TalksSSH:
		if t.Fleet != "" {
			return fmt.Sprintf("fleet %s at %s (%s)", t.Fleet, t.Target, t.Why)
		}
		return fmt.Sprintf("%s (%s)", t.Target, t.Why)
	}
	return "nothing: " + t.Problem
}

func fleetsTable(fs []place.Fleet) *ui.Table {
	t := ui.NewTable("FLEET", "HUB", "KEY", "APPS", "DEFAULT", "TUNNEL")
	for _, f := range fs {
		t.Row(f.Name, f.Hub, strOrDash(remote.ElideKey(f.PublicKey)),
			strOrDash(strings.Join(f.Apps, " ")), yesOrDash(f.Default), yesOrDash(f.Record))
	}
	return t
}

func yesOrDash(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}
