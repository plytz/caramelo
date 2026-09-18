package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/edge"
	"github.com/plytz/caramelo/internal/edge/certs"
	"github.com/plytz/caramelo/internal/env"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func (e *envCmd) exposeCmd() *cobra.Command {
	var service, host string
	cmd := &cobra.Command{
		Use:   "expose ENV",
		Short: "Put an environment's service behind a public hostname",
		Long: `Routes a hostname on this machine's edge to one service of one
environment, with a certificate issued on the first request.

The name comes from the app's 'domain:' — <env>.<domain> for the first exposed
service, <service>.<env>.<domain> for the others — unless --host names another
one, which is also how an app whose configuration has no domain is exposed at
all.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := e.requireApp(); err != nil {
				return err
			}
			req := env.ExposeRequest{App: e.app, Name: args[0], Service: service, Host: host}
			var res *api.ExposeResult
			var err error
			if e.exposeVia == "" {

				res, err = e.service().Expose(cmd.Context(), req)
			} else {

				via, perr := config.ParseVia(e.exposeVia)
				if perr != nil {
					return &usageError{perr}
				}
				req.Via = via
				res, err = e.service().EnvExpose(cmd.Context(),
					api.ExposeRequest{ExposeRequest: req})
			}
			if err != nil {
				return err
			}
			return e.a.printer().Result(res, func(w io.Writer) error {
				return writeExposeResult(w, res, "exposed")
			})
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "service to expose (default: the first one with a port)")
	cmd.Flags().StringVar(&host, "host", "", "hostname to route (default: derived from the app's domain)")
	cmd.Flags().StringVar(&e.exposeVia, "via", "",
		"whose edge serves the name: `member` (the machine it runs on, the default) or `hub`")
	return available(cmd, onCommander.or(onHub))
}

func (e *envCmd) unexposeCmd() *cobra.Command {
	var service, host string
	var force bool
	cmd := &cobra.Command{
		Use:   "unexpose ENV",
		Short: "Stop serving an environment's public hostnames",
		Long: `Removes the environment's routes from the machine's edge: the names stop
resolving to anything within a second, and an unknown host gets the same
treatment as a name this machine never served.

The environment keeps running and stays reachable on the machine's private
network. Its certificates are kept, so exposing it again does not ask the
certificate authority for a new one.

A protected environment needs --force to take every name it serves off the
edge, the way caramelo down and env destroy do: one word is the difference
between unexposing a preview and taking production off the air. Removing a
single name with --host needs no force.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := e.requireApp(); err != nil {
				return err
			}
			res, err := e.service().Unexpose(cmd.Context(), env.UnexposeRequest{
				App: e.app, Name: args[0], Service: service, Host: host, Force: force,
			})
			if err != nil {
				return err
			}
			return e.a.printer().Result(res, func(w io.Writer) error {
				return writeExposeResult(w, res, "no longer exposed")
			})
		},
	}
	cmd.Flags().StringVar(&service, "service", "", "only this service's routes (default: all of the environment's)")
	cmd.Flags().StringVar(&host, "host", "", "only this hostname (default: all of them)")
	cmd.Flags().BoolVar(&force, "force", false,
		"take every name of a protected environment off the edge")
	return available(cmd, onCommander.or(onHub))
}

func writeExposeResult(w io.Writer, r *api.ExposeResult, verb string) error {
	return exposeView(r, verb).Write(w)
}

func exposeView(r *api.ExposeResult, verb string) *ui.View {
	v := ui.NewView()
	if r == nil {
		return v.Text("nothing changed")
	}
	for _, host := range r.Changed {
		v.Text("%s %s", host, verb)
	}
	if len(r.Changed) == 0 {
		v.Text("nothing changed: %s/%s was already %s", r.Env.App, r.Env.Name, verb)
	}
	if len(r.Routes) == 0 {
		return v.Text("%s/%s is not public; it is still reachable on the machine's network",
			r.Env.App, r.Env.Name)
	}
	return v.Section(routesTable(r.Routes))
}

func writeRoutes(w io.Writer, routes []edge.Route) error {
	return ui.NewView().Table(routesTable(routes)).Write(w)
}

func routesTable(routes []edge.Route) *ui.Table {
	if len(routes) == 0 {
		return nil
	}
	head := []string{"URL", "SERVICE", "ACTIVE", "IN-FLIGHT", "SINCE"}
	if routesHaveVia(routes) {
		head = append(head, "VIA")
	}
	t := ui.NewTable(head...)
	for _, r := range routes {
		since := "-"
		if !r.CreatedAt.IsZero() {
			since = r.CreatedAt.Local().Format(time.RFC3339)
		}
		cells := []string{"https://" + r.Host, strOrDash(r.Service),
			fmt.Sprintf("%d/%d", len(r.Active()), len(r.Targets)),
			fmt.Sprint(r.Inflight()), since}
		if routesHaveVia(routes) {
			cells = append(cells, strOrDash(r.Via))
		}
		t.Row(cells...)
	}
	return t
}

func routesHaveVia(routes []edge.Route) bool {
	for _, r := range routes {
		if r.Via != "" {
			return true
		}
	}
	return false
}

func writeCertificates(w io.Writer, list []certs.Certificate) error {
	return ui.NewView().Table(certificatesTable(list)).Write(w)
}

func certificatesTable(list []certs.Certificate) *ui.Table {
	if len(list) == 0 {
		return nil
	}
	t := ui.NewTable("CERTIFICATE", "ISSUER", "EXPIRES", "MANAGED")
	now := time.Now()
	for _, c := range list {
		expires := "-"
		switch {
		case c.NotAfter.IsZero():
		case c.Expired(now):
			expires = c.NotAfter.Local().Format(time.RFC3339) + " (expired)"
		default:
			expires = fmt.Sprintf("%s (in %s)", c.NotAfter.Local().Format(time.RFC3339),
				c.NotAfter.Sub(now).Round(time.Minute))
		}
		t.Row(c.Host, strOrDash(c.Issuer), expires, fmt.Sprintf("%v", c.Managed))
	}
	return t
}
