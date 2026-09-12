package release

import (
	"context"
	"errors"
	"fmt"
	goruntime "runtime"
	"sort"
	"strings"

	"github.com/plytz/caramelo/internal/runtime"
)

type ImageStore interface {
	PutImage(ctx context.Context, im Image) error

	Images(ctx context.Context, releaseID int64) (Images, error)

	RemoveImages(ctx context.Context, releaseID int64, machine string) error
}

type Action string

const (
	PlanHave Action = "have"

	PlanCopy Action = "copy"

	PlanBuild Action = "build"
)

type Source struct {
	Machine   string `json:"machine"`
	Reachable bool   `json:"reachable"`
}

type Need struct {
	Release *Release

	Machine string
	Arch    string

	Services []string

	Present []string

	Known Images

	Sources []Source
}

type Plan struct {
	Action Action `json:"action"`

	Machine string `json:"machine,omitempty"`
	From    string `json:"from,omitempty"`

	Refs     []string `json:"refs,omitempty"`
	Services []string `json:"services,omitempty"`

	Why string `json:"why,omitempty"`

	Candidates []string `json:"candidates,omitempty"`
}

func Decide(n Need) (Plan, error) {
	if n.Release == nil {
		return Plan{}, fmt.Errorf("plan the images of a deploy onto machine %q: no release", n.Machine)
	}
	rel := n.Release
	if len(rel.Images) == 0 {
		return Plan{}, fmt.Errorf("release %s of app %q has no images recorded: it has to be built again",
			rel.Short(), rel.App)
	}
	if strings.TrimSpace(n.Arch) == "" {
		return Plan{}, fmt.Errorf("plan the images of release %s onto machine %q: "+
			"it has not said what architecture it is, and an image of the wrong one loads and then cannot run",
			rel.Short(), n.Machine)
	}
	services, err := wanted(rel, n.Services)
	if err != nil {
		return Plan{}, err
	}
	here := map[string]bool{}
	for _, ref := range n.Present {
		here[ref] = true
	}
	var missing, missingRefs []string
	for _, s := range services {
		ref := rel.Images[s]
		if here[ref] {
			continue
		}
		missing = append(missing, s)
		missingRefs = append(missingRefs, ref)
	}
	plan := Plan{Machine: n.Machine, Refs: missingRefs, Services: missing}
	if len(missing) == 0 {
		plan.Action = PlanHave
		plan.Why = fmt.Sprintf("%s holds every image of release %s", where(n.Machine), rel.Short())
		plan.Refs, plan.Services = nil, nil
		return plan, nil
	}
	plan.Candidates = holders(n, missing)
	if len(plan.Candidates) > 0 {
		plan.Action, plan.From = PlanCopy, plan.Candidates[0]
		plan.Why = fmt.Sprintf("copying %s of release %s from %s (%s)",
			plural(len(missingRefs), "image"), rel.Short(), plan.From, n.Arch)
		return plan, nil
	}
	plan.Action = PlanBuild
	plan.Why = fmt.Sprintf("no machine of %s holds %s of release %s: building %s",
		n.Arch, plural(len(missingRefs), "image"), rel.Short(), where(n.Machine))
	return plan, nil
}

func wanted(rel *Release, only []string) ([]string, error) {
	all := rel.Services()
	if len(only) == 0 {
		return all, nil
	}
	have := map[string]bool{}
	for _, s := range all {
		have[s] = true
	}
	out := make([]string, 0, len(only))
	for _, s := range only {
		if !have[s] {
			return nil, fmt.Errorf("release %s has no service %q: it has %s", rel.Short(), s, quoted(all))
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

func holders(n Need, missing []string) []string {
	reachable := map[string]bool{}
	for _, s := range n.Sources {
		if s.Machine != "" && s.Machine != n.Machine {
			reachable[s.Machine] = s.Reachable
		}
	}
	var out []string
	for _, m := range n.Known.Machines(n.Arch, missing) {
		if reachable[m] {
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

func where(machine string) string {
	if strings.TrimSpace(machine) == "" {
		return "this machine"
	}
	return machine
}

func ServiceOf(ref string) string {
	name, _, ok := strings.Cut(ref, ":")
	if !ok {
		return ""
	}
	parts := strings.Split(name, "/")
	if len(parts) != 3 || parts[0] != NamePrefix || parts[1] == "" || parts[2] == "" {
		return ""
	}
	return parts[2]
}

func Attach(ctx context.Context, store ImageStore, releases ...*Release) error {
	if store == nil {
		return nil
	}
	for _, rel := range releases {
		if rel == nil || rel.ID == 0 {
			continue
		}
		images, err := store.Images(ctx, rel.ID)
		if err != nil {
			return fmt.Errorf("read the images of release %s: %w", rel.Short(), err)
		}
		images.Sort()
		rel.Built = images
	}
	return nil
}

func (b *Docker) recordImages(ctx context.Context, rel *Release) error {
	if rel == nil {
		return nil
	}
	rel.Machine = b.Machine
	if b.Images == nil {
		return nil
	}
	arch := b.arch()
	rows := make(Images, 0, len(rel.Images))
	for _, service := range rel.Services() {
		ref := rel.Images[service]
		row := Image{
			ReleaseID: rel.ID, Service: service, Ref: ref, Arch: arch,
			Machine: b.Machine, BuiltAt: b.now(),
		}

		if s, err := runtime.Streamer(b.Driver); err == nil {
			info, err := s.ImageInfo(ctx, ref)
			switch {
			case errors.Is(err, runtime.ErrNotFound):

				continue
			case err != nil:
				return fmt.Errorf("look for image %s on %s: %w", ref, where(b.Machine), err)
			}
			row.ID = info.ID
			if info.Arch != "" {
				row.Arch = info.Arch
			}
		}
		rows = append(rows, row)
	}
	for _, row := range rows {
		if err := b.Images.PutImage(ctx, row); err != nil {
			return fmt.Errorf("record image %s of release %s on %s: %w",
				row.Ref, rel.Short(), where(b.Machine), err)
		}
	}
	all, err := b.Images.Images(ctx, rel.ID)
	if err != nil {
		return fmt.Errorf("read the images of release %s: %w", rel.Short(), err)
	}
	all.Sort()
	rel.Built = all
	return nil
}

func (b *Docker) arch() string {
	if a := strings.TrimSpace(b.Arch); a != "" {
		return a
	}
	return goruntime.GOARCH
}
