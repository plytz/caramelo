package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/vpnclient"
)

func init() {
	register(func(a *app) *cobra.Command {
		var listen, appName string
		cmd := &cobra.Command{
			Use:   "connect ENV [SERVICE|DEP ...]",
			Short: "Open local ports for an environment's services and dependencies",
			Long: `Opens a local port for each of the environment's services and
dependencies and keeps them open until interrupted. Use it in the default
userspace mode, when a program that is not Caramelo — a browser, psql, a game
client — has to reach an environment. With no names, everything the environment
has is published; with names, only those.

The port map is printed on stdout, and with --json as one document, so an agent
can read the ports it was given.`,
			Args: minArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				machine, err := a.vpnFleet()
				if err != nil {
					return err
				}
				envName := args[0]
				if err := env.ValidateName("environment", envName); err != nil {
					return &usageError{err}
				}
				app := strings.TrimSpace(appName)
				if app == "" {
					app = appFromCheckout(cmd.Context(), ".")
				}
				if app == "" {
					return &usageError{errors.New(
						"cannot tell which app this is: run this inside the app's checkout, " +
							"or name it with --app NAME (or CARAMELO_APP)")}
				}
				if err := env.ValidateName("app", app); err != nil {
					return &usageError{err}
				}

				client, err := a.vpnClient()
				if err != nil {
					return err
				}
				dialer, err := client.Dialer(cmd.Context(), machine)
				if err != nil {
					return noKeyError(machine, err)
				}
				defer func() { _ = dialer.Close() }()

				connector, err := vpnclient.NewConnector(vpnclient.ConnectorOptions{
					Dialer: dialer,
					Lookup: &vpnclient.ControlLookup{Control: fleetControl{a: a}},
					Log:    a.stderr,
				})
				if err != nil {
					return err
				}
				defer func() { _ = connector.Close() }()

				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
				defer stop()

				listeners, err := connector.Open(ctx, vpnclient.ConnectRequest{
					Machine: machine,
					App:     app,
					Env:     envName,
					Listen:  listen,
					Only:    args[1:],
				})
				if err != nil {
					return err
				}
				out := struct {
					App       string               `json:"app"`
					Env       string               `json:"env"`
					Machine   string               `json:"machine"`
					Listeners []vpnclient.Listener `json:"listeners"`
				}{app, envName, machine, listeners}
				if err := a.printer().Result(out, func(w io.Writer) error {
					return renderListeners(w, app, envName, listeners)
				}); err != nil {
					return err
				}

				a.printer().Infof("connected; press Ctrl-C to close these ports")
				<-ctx.Done()
				a.printer().Infof("closing %d ports", len(listeners))
				return nil
			},
		}
		cmd.Flags().StringVar(&listen, "listen", "127.0.0.1",
			"local address to bind the ports on")
		cmd.Flags().StringVar(&appName, "app", os.Getenv("CARAMELO_APP"),
			"application the environment belongs to (default: the checkout you are in)")
		return cmd
	})
}

func noKeyError(machine string, err error) error {
	if errors.Is(err, vpnclient.ErrNoKey) {
		return fmt.Errorf("the commander has not joined the network of %s: run 'caramelo vpn up'", machine)
	}
	return err
}

func renderListeners(w io.Writer, app, envName string, listeners []vpnclient.Listener) error {
	if len(listeners) == 0 {
		_, err := fmt.Fprintf(w, "%s/%s publishes nothing\n", app, envName)
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tKIND\tPROTO\tLOCAL\tIN THE TUNNEL")
	for _, l := range listeners {
		target := fmt.Sprintf("%s:%d", l.Host, portOfRemote(l))
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", l.Name, l.Kind, l.Protocol, l.Local, target)
	}
	return tw.Flush()
}

func portOfRemote(l vpnclient.Listener) int {
	i := strings.LastIndex(l.Remote, ":")
	if i < 0 {
		return 0
	}
	var port int
	if _, err := fmt.Sscanf(l.Remote[i+1:], "%d", &port); err != nil {
		return 0
	}
	return port
}
