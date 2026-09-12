package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/vpnclient"

	"github.com/plytz/caramelo/internal/cli/ui"
)

func init() {
	register(func(a *app) *cobra.Command {
		t := &envTargets{a: a}
		var (
			services []string
			build    bool
			noPush   bool
			force    bool
			noWait   bool
			timeout  time.Duration
		)
		cmd := &cobra.Command{
			Use:   "up [ENV]",
			Short: "Start an environment's services and wait for them to be healthy",
			Long: `up runs the app inside an environment: it pushes the code you are
standing on to the environment's branch, works out how the app is built and
run (caramelo.yaml, filled in by stack detection), builds or pulls the image,
puts every service on the environment's own network with its dependencies,
and waits for each one to answer.

It is idempotent: a service whose image, command, variables and mounts have
not changed and whose container is running is left alone.

The services run in containers with the environment's worktree mounted, so an
edit is live without a rebuild. Nothing is ever installed on the machine.

The push is refused when the environment's worktree has uncommitted changes:
work someone is doing in the environment is never overwritten. Commit it there
(caramelo env exec ENV -- git status), or pass --no-push.`,
			Args: maxArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				name, _, err := t.target(cmd, args)
				if err != nil {
					return err
				}
				res, err := t.service().Up(cmd.Context(), env.UpRequest{
					App:      t.app,
					Name:     name,
					Services: services,
					Build:    build,
					Timeout:  timeout,
					NoWait:   noWait,
				}, t.a.progressWriter())
				if err != nil {

					if res != nil && t.a.json {

						_ = t.a.printer().Result(res, func(io.Writer) error { return nil })
					}
					return err
				}
				return t.a.printer().Result(res, func(w io.Writer) error {
					return writeUpResult(w, res)
				})
			},
		}
		t.beforeForward = func(cmd *cobra.Command) error { return t.pushBeforeUp(cmd, noPush, force) }
		t.addTargetFlags(cmd)
		cmd.Flags().StringSliceVar(&services, "service", nil,
			"only these services (default: all of them)")
		cmd.Flags().BoolVar(&build, "build", false, "rebuild the image even when the tree has not changed")
		cmd.Flags().BoolVar(&noPush, "no-push", false, "do not push the current checkout first")
		cmd.Flags().BoolVar(&force, "force", false, "force-push the branch")
		cmd.Flags().BoolVar(&noWait, "no-wait", false, "start the services without waiting for health")
		cmd.Flags().DurationVar(&timeout, "timeout", 0, "how long to wait for every service to become healthy (default 2m)")
		return cmd
	})
}

func (t *envTargets) pushBeforeUp(cmd *cobra.Command, noPush, force bool) error {
	if noPush {
		return nil
	}
	tr, err := resolveTransport(cmd.Context(), t.a)
	if err != nil {

		return nil
	}
	if tr.kind != kindSSH && tr.kind != kindTunnel {
		return nil
	}
	if _, err := runGit(cmd.Context(), ".", "rev-parse", "--git-dir"); err != nil {
		return nil
	}
	plan := pushPlan{
		Dir:    ".",
		Remote: pushURL(tr.target, t.app),
		Ref:    "HEAD:" + t.env,
		Force:  force,
		Tunnel: tr.kind == kindTunnel,
	}
	t.a.printer().Infof("%s", plan)
	if plan.Tunnel {

		vpnclient.CloseTunnels()
	}
	return gitPush(cmd.Context(), plan, t.a.stderr)
}

func writeUpResult(w io.Writer, r *api.UpResult) error { return upView(r).Write(w) }

func upView(r *api.UpResult) *ui.View {
	if r == nil {
		return ui.NewView().Text("nothing to start")
	}
	e := r.Env
	f := ui.NewFields(fmt.Sprintf("%s/%s  %s", e.App, e.Name, e.Status))
	if r.Stack != "" {
		f.Add("stack", "%s", r.Stack)
	}
	if r.Image != "" {
		f.Add("image", "%s", r.Image)
	}
	if r.Network != "" {
		f.Add("network", "%s", r.Network)
	}
	v := ui.NewView().Fields(f)
	if len(r.Services) == 0 {
		return v.Blank().Text("no services: caramelo.yaml describes none and nothing was detected")
	}
	v.Section(servicesTable(r.Services)).Section(replicasTable(r.Services))

	v.Section(rolloutsTable(r.Rollouts)).Section(routesTable(r.Routes))
	if r.URL != "" {
		v.Blank().Text("%s", r.URL)
	}
	return v
}

func writeRollouts(w io.Writer, rollouts []env.Rollout) error {
	return ui.NewView().Table(rolloutsTable(rollouts)).Write(w)
}

func rolloutsTable(rollouts []env.Rollout) *ui.Table {
	t := ui.NewTable("ROLLOUT", "HOST", "REPLICAS", "STEPS", "RESULT")
	for _, r := range rollouts {
		result := "ok"
		if r.Failed != nil {
			result = fmt.Sprintf("failed at %s of replica %d", r.Failed.Step, r.Failed.Replica)
		}
		t.Row(r.Service, strOrDash(r.Host), fmt.Sprint(len(r.Replicas)), fmt.Sprint(len(r.Steps)), result)
	}
	for _, r := range rollouts {
		if r.Failed != nil && r.Failed.Detail != "" {
			t.Note("\n%s/%d %s: %s", r.Service, r.Failed.Replica, r.Failed.Step, r.Failed.Detail)
		}
	}
	return t
}

func writeReplicas(w io.Writer, services []env.Service) error {
	return ui.NewView().Section(replicasTable(services)).Write(w)
}

func replicasTable(services []env.Service) *ui.Table {
	var interesting []env.Service
	for _, s := range services {
		if len(s.Replicas) > 1 || (len(s.Replicas) > 0 && s.Host != "") {
			interesting = append(interesting, s)
		}
	}
	if len(interesting) == 0 {
		return nil
	}
	t := ui.NewTable("REPLICA", "STATE", "STATUS", "HEALTH", "PORT", "IN-FLIGHT", "RESTARTS", "SINCE")
	for _, s := range interesting {
		for _, r := range s.Replicas {
			since := "-"
			if !r.Since.IsZero() {
				since = time.Since(r.Since).Round(time.Second).String()
			}
			t.Row(fmt.Sprintf("%s/%d", s.Name, r.Index), strOrDash(string(r.State)),
				strOrDash(string(r.Status)), strOrDash(string(r.Health)),
				portOrDash(r.Port), fmt.Sprint(r.Inflight), fmt.Sprint(r.Restarts), since)
		}
	}
	for _, s := range interesting {
		for _, r := range s.Replicas {
			if r.Detail != "" {
				t.Note("\n%s/%d: %s", s.Name, r.Index, r.Detail)
			}
		}
	}
	return t
}

func portOrDash(port int) string {
	if port == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", port)
}

func writeServices(w io.Writer, services []env.Service) error {
	return ui.NewView().Table(servicesTable(services)).Write(w)
}

func servicesTable(services []env.Service) *ui.Table {
	t := ui.NewTable("SERVICE", "STATUS", "HEALTH", "CHANGE", "URL", "IMAGE")
	for _, s := range services {
		t.Row(s.Name, strOrDash(string(s.Status)), strOrDash(string(s.Health)),
			strOrDash(string(s.Change)), strOrDash(s.URL), strOrDash(s.Image))
	}
	for _, s := range services {
		if s.Detail != "" {
			t.Note("\n%s: %s", s.Name, s.Detail)
		}
	}
	return t
}
