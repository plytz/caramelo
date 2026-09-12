package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/plytz/caramelo/internal/api"
	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/vault"
)

func init() {
	registerRender("build", renderResult(writeBuildResult))
	registerRender("deploy", renderResult(writeDeployResult))
	registerRender("promote", renderResult(writeDeployResult))
	registerRender("rollback", renderResult(writeDeployResult))
	registerRender("releases", renderResult(writeReleasesResult))
	registerRender("secrets set", renderResult(writeVaultResult))
	registerRender("secrets list", renderResult(writeVaultResult))
	registerRender("secrets rm", renderResult(writeVaultResult))
	registerRender("secrets export", renderResult(writeVaultExport))
	registerRender("edge counts", renderResult(writeEdgeCounts))
}

func renderResult[T any](fn func(io.Writer, *T) error) Renderer {
	return func(w io.Writer, raw json.RawMessage) error {
		var v *T
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("decode the daemon's answer: %w", err)
		}
		return fn(w, v)
	}
}

func writeBuildResult(w io.Writer, r *api.BuildResult) error {
	if r == nil || r.Release == nil {
		_, err := fmt.Fprintln(w, "no release")
		return err
	}
	rel := r.Release
	what := "built"
	if !r.Built {
		what = "unchanged"
	}
	fmt.Fprintf(w, "%s/%s  release %s  %s\n", r.App, r.Env, rel.Short(), what)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  commit\t%s\n", shortCommit(rel.Commit))
	if rel.Ref != "" {
		fmt.Fprintf(tw, "  ref\t%s\n", rel.Ref)
	}
	fmt.Fprintf(tw, "  tree\t%s\n", release.ShortTree(rel.Tree))
	if rel.BuiltBy != "" {
		fmt.Fprintf(tw, "  built by\t%s\n", rel.BuiltBy)
	}
	if !rel.BuiltAt.IsZero() {
		fmt.Fprintf(tw, "  built\t%s\n", rel.BuiltAt.Local().Format(time.RFC3339))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("render release: %w", err)
	}
	images := r.Images
	if len(images) == 0 {
		images = rel.ImageList()
	}
	if len(images) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	it := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(it, "SERVICE\tIMAGE")
	for _, service := range rel.Services() {
		image, _ := rel.Image(service)
		fmt.Fprintf(it, "%s\t%s\n", service, image)
	}
	return it.Flush()
}

func writeDeployResult(w io.Writer, r *api.DeployResult) error {
	if r == nil || r.Deploy == nil {
		_, err := fmt.Fprintln(w, "no deploy")
		return err
	}
	d := r.Deploy
	fmt.Fprintf(w, "%s/%s  %s %s", d.App, d.Env, d.Kind, d.Status)
	if d.Release != nil {
		fmt.Fprintf(w, "  release %s", d.Release.Short())
	}
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if d.FromRelease != nil {
		fmt.Fprintf(tw, "  from\t%s\n", d.FromRelease.Short())
	}
	if d.Reason != "" {
		fmt.Fprintf(tw, "  reason\t%s\n", d.Reason)
	}
	if d.Identity != "" {
		fmt.Fprintf(tw, "  by\t%s\n", d.Identity)
	}
	if !d.StartedAt.IsZero() {
		fmt.Fprintf(tw, "  started\t%s\n", d.StartedAt.Local().Format(time.RFC3339))
	}
	if !d.FinishedAt.IsZero() {
		fmt.Fprintf(tw, "  finished\t%s  (%s)\n", d.FinishedAt.Local().Format(time.RFC3339),
			d.FinishedAt.Sub(d.StartedAt).Round(time.Second))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("render deploy: %w", err)
	}

	if len(d.Steps) > 0 {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		st := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(st, "STEP\tSERVICE\tSTATUS\tTOOK\tDETAIL")
		for _, s := range d.Steps {
			took := "-"
			if s.Duration > 0 {
				took = s.Duration.Round(time.Millisecond).String()
			}
			fmt.Fprintf(st, "%s\t%s\t%s\t%s\t%s\n",
				s.Step, strOrDash(s.Service), strOrDash(string(s.Status)), took, s.Detail)
		}
		if err := st.Flush(); err != nil {
			return fmt.Errorf("render deploy steps: %w", err)
		}
	}

	if d.Watch != nil {
		if _, err := fmt.Fprintf(w, "\n%s\n", describeWatch(*d.Watch)); err != nil {
			return err
		}
	}
	if len(d.Rollouts) > 0 {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		if err := writeRollouts(w, d.Rollouts); err != nil {
			return err
		}
	}
	if d.URL != "" {
		if _, err := fmt.Fprintf(w, "\n%s\n", d.URL); err != nil {
			return err
		}
	} else if len(d.Routes) > 0 {
		if _, err := fmt.Fprintf(w, "\nserving %s\n", strings.Join(d.Routes, ", ")); err != nil {
			return err
		}
	}
	if d.Error != "" {
		if _, err := fmt.Fprintf(w, "\n%s\n", d.Error); err != nil {
			return err
		}
	}
	return nil
}

func describeWatch(watch env.DeployWatch) string {
	line := fmt.Sprintf("watch %s of %s: %d request(s), %d error(s)",
		watch.Elapsed.Round(time.Second), watch.Window.Round(time.Second), watch.Requests, watch.Errors)
	if watch.Requests > 0 {
		line += fmt.Sprintf(", %.1f%%", watch.Rate*100)
	}
	switch {
	case watch.MaxRate <= 0:
		line += " (no error budget set)"
	case watch.Requests < watch.MinRequests:
		line += fmt.Sprintf(" — under the %d-request floor, so health decides", watch.MinRequests)
	case watch.Breached():
		line += fmt.Sprintf(" — over the %.1f%% deploy.max_errors allows", watch.MaxRate*100)
	default:
		line += fmt.Sprintf(" of the %.1f%% deploy.max_errors allows", watch.MaxRate*100)
	}
	return line
}

func writeReleasesResult(w io.Writer, r *api.ReleasesResult) error {
	if r == nil {
		_, err := fmt.Fprintln(w, "no history")
		return err
	}
	if r.Env == "" {

		return writeAppReleases(w, r)
	}
	if len(r.Deploys) == 0 {
		_, err := fmt.Fprintf(w, "%s/%s has never been deployed\n", r.App, r.Env)
		return err
	}
	current := ""
	if r.Current != nil {
		current = r.Current.Short()
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RELEASE\tKIND\tSTATUS\tCOMMIT\tSTARTED\tTOOK\tBY\tREASON")
	for _, d := range r.Deploys {
		name, commit := "-", "-"
		if d.Release != nil {
			name, commit = d.Release.Short(), shortCommit(d.Release.Commit)
			if name == current {
				name += " *"
			}
		}
		started := "-"
		if !d.StartedAt.IsZero() {
			started = d.StartedAt.Local().Format(time.RFC3339)
		}
		took := "-"
		if !d.FinishedAt.IsZero() && !d.StartedAt.IsZero() {
			took = d.FinishedAt.Sub(d.StartedAt).Round(time.Second).String()
		}
		reason := d.Reason
		if d.Error != "" {
			reason = strings.TrimSpace(reason + " " + d.Error)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			name, d.Kind, d.Status, commit, started, took, strOrDash(d.Identity), strOrDash(reason))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("render releases: %w", err)
	}
	if current != "" {
		if _, err := fmt.Fprintf(w, "\n* %s is what %s/%s is running\n", current, r.App, r.Env); err != nil {
			return err
		}
	}
	return nil
}

func writeVaultResult(w io.Writer, r *api.VaultResult) error {
	if r == nil {
		_, err := fmt.Fprintln(w, "no vault")
		return err
	}
	if len(r.Changed) > 0 {
		if _, err := fmt.Fprintf(w, "%s\n", strings.Join(r.Changed, ", ")); err != nil {
			return err
		}
	}
	defaults := defaultScopeNames(r)
	if len(r.Entries) == 0 && len(defaults) == 0 {
		if _, err := fmt.Fprintln(w, "no secrets"); err != nil {
			return err
		}
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SCOPE\tNAME\tWHERE\tVERSION\tUPDATED\tBY\tWINS")
		for _, e := range r.Entries {
			updated := "-"
			if !e.UpdatedAt.IsZero() {
				updated = e.UpdatedAt.Local().Format(time.RFC3339)
			}
			wins := ""
			if from, ok := r.Resolved[e.Name]; ok && from == e.Scope {
				wins = "*"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n",
				e.Scope, e.Name, strOrDash(e.Where()), e.Version, updated, strOrDash(e.UpdatedBy), wins)
		}

		for _, name := range defaults {
			fmt.Fprintf(tw, "%s\t%s\t%s\t-\t-\t-\t*\n",
				vault.ScopeDefault, name, "the image's default")
		}
		if err := tw.Flush(); err != nil {
			return fmt.Errorf("render secrets: %w", err)
		}
		if len(r.Resolved) > 0 {
			if _, err := fmt.Fprintf(w,
				"\n* is the value %s/%s resolves; the narrowest scope wins\n", r.App, r.Env); err != nil {
				return err
			}
		}
	}
	for _, warning := range r.Warnings {
		if _, err := fmt.Fprintf(w, "\n%s\n", warning); err != nil {
			return err
		}
	}
	return nil
}

func defaultScopeNames(r *api.VaultResult) []string {
	if len(r.Resolved) == 0 {
		return nil
	}
	stored := make(map[string]bool, len(r.Entries))
	for _, e := range r.Entries {
		stored[e.Name] = true
	}
	var out []string
	for name, scope := range r.Resolved {
		if scope == vault.ScopeDefault && !stored[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func writeVaultExport(w io.Writer, r *api.VaultExportResult) error {
	if r == nil {
		_, err := fmt.Fprintln(w, "no secrets")
		return err
	}
	text := r.Text
	if text == "" {
		text = renderEnvFile(r.Values)
	}
	if _, err := io.WriteString(w, text); err != nil {
		return err
	}
	if r.Revealed || len(r.Values) == 0 {
		return nil
	}

	_, err := fmt.Fprintf(w, "\n# %d value(s) redacted; --reveal prints them, and is recorded as an event\n",
		len(r.Values))
	return err
}

func writeAppReleases(w io.Writer, r *api.ReleasesResult) error {
	if len(r.Releases) == 0 {
		_, err := fmt.Fprintf(w, "%s has no releases\n", r.App)
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RELEASE\tCOMMIT\tBUILT ON\tARCH\tBUILT\tBY")
	for i := range r.Releases {
		rel := &r.Releases[i]
		built := "-"
		if !rel.BuiltAt.IsZero() {
			built = rel.BuiltAt.Local().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			rel.Short(), shortCommit(rel.Commit), dash(rel.Machine),
			dash(strings.Join(rel.Arches(), ",")), built, dash(rel.BuiltBy))
	}
	return tw.Flush()
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
