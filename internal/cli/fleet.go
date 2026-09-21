package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/cli/ui"
	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/vpnclient"
)

func init() {
	register(func(a *app) *cobra.Command {
		cmd := &cobra.Command{
			Use:   "fleet",
			Short: "Manage the fleets this commander talks to",
			Long: `A fleet is a hub and every machine that joined it. A server belongs to one
fleet; a commander knows as many as it has been told about and talks to one at a
time.

'fleet setup' makes one: it turns a machine into the hub the rest of the fleet
joins, on the box itself as root or from here over ssh with --target. The other
commands are about the fleets this commander already knows.

A fleet is recorded here by the ssh address of its hub and by the hub's public
key, which is the fleet's real identity: the key is pinned the first time this
commander reaches the fleet, and a hub answering at the same address with another
key is refused rather than adopted.

Which fleet a command talks to is decided in this order: --fleet (or
CARAMELO_FLEET), the fleet recorded for the app of the checkout you are standing
in, commander.default_fleet, and the only fleet when there is exactly one. With
several fleets and none of those, a command refuses rather than guess.`,
		}
		asGroup(cmd)
		cmd.AddCommand(a.fleetSetupCmd(), a.fleetListCmd(), a.fleetAddCmd(),
			a.fleetRemoveCmd(), a.fleetDefaultCmd())
		return cmd
	})
}

type fleetView struct {
	Name      string   `json:"name"`
	Hub       string   `json:"hub"`
	PublicKey string   `json:"public_key,omitempty"`
	Apps      []string `json:"apps,omitempty"`
	Default   bool     `json:"default"`
}

func fleetViews(cfg remote.CommanderConfig) []fleetView {
	out := make([]fleetView, 0, len(cfg.Commander.Fleets))
	for _, name := range cfg.FleetNames() {
		f := cfg.Commander.Fleets[name]
		apps := append([]string(nil), f.Apps...)
		sort.Strings(apps)
		out = append(out, fleetView{
			Name: name, Hub: f.Hub, PublicKey: f.PublicKey, Apps: apps,
			Default: cfg.Commander.DefaultFleet == name,
		})
	}
	return out
}

func fleetsView(fs []fleetView) *ui.View {
	if len(fs) == 0 {
		return ui.NewView().Text(
			"no fleets: record one with `caramelo fleet add NAME user@host`, " +
				"or set a box up with `caramelo fleet setup --target user@host`")
	}
	t := ui.NewTable("NAME", "HUB", "KEY", "APPS", "DEFAULT")
	for _, f := range fs {
		mark := "-"
		if f.Default {
			mark = "yes"
		}
		t.Row(f.Name, f.Hub, strOrDash(remote.ElideKey(f.PublicKey)),
			strOrDash(strings.Join(f.Apps, " ")), mark)
	}
	return ui.NewView().Table(t)
}

func (a *app) fleetListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the fleets this commander knows",
		Long: `list shows every fleet in the commander config: its hub, the key pinned for
it, the apps this commander has seen there, and which one is the default.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := remote.LoadCommanderConfig()
			if err != nil {
				return err
			}
			fs := fleetViews(cfg)
			return a.printer().Result(fs, func(w io.Writer) error {
				return fleetsView(fs).Write(w)
			})
		},
	}
	return available(cmd, onCommander)
}

func (a *app) fleetAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add NAME HUB",
		Short: "Record a fleet by name and the ssh address of its hub",
		Long: `add writes a fleet into the commander config. HUB is the hub's ssh address
(user@host[:port]); the user defaults to the machine's caramelo user and the port
to its API port.

Adding a fleet that is already there moves its hub address and keeps the key
pinned for it, so a hub that changed address is still the same fleet and a hub
that changed key is still refused. The first fleet recorded becomes the default.`,
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, hub := args[0], args[1]
			if !remote.ValidFleetName(name) {
				return &usageError{fmt.Errorf(
					"fleet %q: a fleet's name is a slug — lowercase letters, digits and dashes", name)}
			}
			target, err := remote.ParseTarget(hub)
			if err != nil {
				return &usageError{err}
			}
			path, err := remote.CommanderConfigPath()
			if err != nil {
				return err
			}
			cfg, err := remote.LoadCommanderConfigFrom(path)
			if err != nil {
				return err
			}
			f := cfg.Commander.Fleets[name]
			f.Hub = target.String()
			cfg.SetFleet(name, f)
			if err := remote.SaveCommanderConfigTo(path, cfg); err != nil {
				return err
			}
			out := fleetView{
				Name: name, Hub: f.Hub, PublicKey: f.PublicKey, Apps: f.Apps,
				Default: cfg.Commander.DefaultFleet == name,
			}
			return a.printer().Result(out, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "fleet %s recorded: hub %s%s\n",
					out.Name, out.Hub, defaultSuffix(out.Default))
				return err
			})
		},
	}
	return available(cmd, onCommander)
}

func defaultSuffix(isDefault bool) string {
	if isDefault {
		return " (the default fleet)"
	}
	return ""
}

func (a *app) fleetRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "remove NAME",
		Aliases: []string{"rm"},
		Short:   "Forget a fleet",
		Long: `remove takes a fleet out of the commander config: its hub address, the key
pinned for it and the apps recorded there. The tunnel this commander kept for the
fleet goes with it, record and key, so a hub rebuilt at the same address is
adopted afresh when the fleet is added again. The fleet itself is untouched.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			path, err := remote.CommanderConfigPath()
			if err != nil {
				return err
			}
			cfg, err := remote.LoadCommanderConfigFrom(path)
			if err != nil {
				return err
			}
			if !cfg.RemoveFleet(name) {
				return &usageError{fmt.Errorf(
					"fleet %q is not one of the fleets this commander knows (%s)", name, cfg.FleetList())}
			}
			if err := forgetFleetTunnel(name); err != nil {
				return err
			}
			if err := remote.SaveCommanderConfigTo(path, cfg); err != nil {
				return err
			}
			out := removed{Name: name, Removed: true}
			return a.printer().Result(out, func(w io.Writer) error {
				return removedView("fleet", out).Write(w)
			})
		},
	}
	return available(cmd, onCommander)
}

func forgetFleetTunnel(name string) error {
	if err := (pinnedRecords{}).Remove(name); err != nil {
		return fmt.Errorf("forget the tunnel this commander kept for fleet %s: %w", name, err)
	}
	if err := (&vpnclient.FileKeyStore{}).Remove(name); err != nil {
		return fmt.Errorf("forget the key this commander used on fleet %s: %w", name, err)
	}
	return nil
}

func (a *app) fleetDefaultCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "default NAME",
		Short: "Make a fleet the one commands talk to when nothing says otherwise",
		Long: `default sets commander.default_fleet. It is what a command uses when --fleet
says nothing and the checkout names no app this commander has seen on a fleet.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			path, err := remote.CommanderConfigPath()
			if err != nil {
				return err
			}
			cfg, err := remote.LoadCommanderConfigFrom(path)
			if err != nil {
				return err
			}
			if err := cfg.SetDefaultFleet(name); err != nil {
				return &usageError{err}
			}
			if err := remote.SaveCommanderConfigTo(path, cfg); err != nil {
				return err
			}
			out := fleetView{
				Name:    name,
				Hub:     cfg.Commander.Fleets[name].Hub,
				Default: true,
			}
			return a.printer().Result(out, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "fleet %s is now the default\n", name)
				return err
			})
		},
	}
	return available(cmd, onCommander)
}
