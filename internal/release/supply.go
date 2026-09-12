package release

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/plytz/caramelo/internal/progress"
	"github.com/plytz/caramelo/internal/runtime"
)

type Puller interface {
	PullImages(ctx context.Context, from string, req TransferRequest) (io.ReadCloser, error)
}

type Supply struct {
	Images *Transfer

	Builder Builder

	Puller Puller

	Lookup func(ctx context.Context, app, tree string) (*Release, error)

	Adopt func(ctx context.Context, rel *Release) (*Release, error)

	Report func(ctx context.Context, rel *Release, images Images) error

	Sources func(ctx context.Context) ([]Source, error)
}

type EnsureRequest struct {
	App string `json:"app"`
	Env string `json:"env"`

	Release *Release `json:"release,omitempty"`

	Ref string `json:"ref,omitempty"`

	Services []string `json:"services,omitempty"`

	Force bool `json:"force,omitempty"`
}

type EnsureResult struct {
	Release *Release `json:"release"`

	Plan Plan `json:"plan"`

	Built  bool            `json:"built"`
	Copied *TransferResult `json:"copied,omitempty"`
}

func (s *Supply) Ensure(ctx context.Context, req EnsureRequest, out io.Writer) (*EnsureResult, error) {
	if s.Builder == nil {
		return nil, fmt.Errorf("deploy %s of app %q: this machine cannot build or copy releases",
			req.Env, req.App)
	}

	fromHub := false
	if req.Release == nil && !req.Force && s.Images != nil {
		if rel := s.lookup(ctx, req, out); rel != nil {
			req.Release, fromHub = rel, true
		}
	}

	if req.Release == nil || req.Force || s.Images == nil {
		res, err := s.build(ctx, req, Plan{Action: PlanBuild, Machine: s.machine(),
			Why: "building here"}, out)
		if err == nil {
			s.report(ctx, res.Release, out)
		}
		return res, err
	}
	plan, err := s.plan(ctx, req, fromHub)
	if err != nil {
		return nil, err
	}
	emit(out, progress.Event{
		App: req.App, Env: req.Env, Action: ActionBuild, Step: string(plan.Action),
		Status: progress.StatusStarted, Detail: plan.Why,
	})
	switch plan.Action {
	case PlanHave:
		return &EnsureResult{Release: req.Release, Plan: plan}, nil
	case PlanCopy:
		res, err := s.copy(ctx, req, plan, out)
		if err == nil {
			s.report(ctx, res.Release, out)
			return res, nil
		}

		emit(out, progress.Event{
			App: req.App, Env: req.Env, Action: ActionBuild, Step: string(PlanCopy),
			Status: progress.StatusWarning,
			Detail: fmt.Sprintf("could not copy from %s: %v — building instead", plan.From, err),
		})
		plan.Action, plan.From = PlanBuild, ""
	}

	res, err := s.build(ctx, req, plan, out)
	if err == nil {
		s.report(ctx, res.Release, out)
	}
	return res, err
}

func (s *Supply) plan(ctx context.Context, req EnsureRequest, fromHub bool) (Plan, error) {
	need := Need{
		Release: req.Release, Machine: s.machine(), Arch: s.Images.Arch,
		Services: req.Services,
	}
	services, err := wanted(req.Release, req.Services)
	if err != nil {
		return Plan{}, err
	}
	for _, svc := range services {
		ref := req.Release.Images[svc]
		_, err := s.Images.Mover.ImageInfo(ctx, ref)
		switch {
		case errors.Is(err, runtime.ErrNotFound):
		case err != nil:
			return Plan{}, fmt.Errorf("look for image %s on %s: %w", ref, where(s.machine()), err)
		default:
			need.Present = append(need.Present, ref)
		}
	}

	need.Known = req.Release.Built
	if s.Images.Store != nil && !fromHub {
		known, err := s.Images.Store.Images(ctx, req.Release.ID)
		if err != nil {
			return Plan{}, fmt.Errorf("read the images of release %s: %w", req.Release.Short(), err)
		}
		need.Known = mergeImages(need.Known, known)
	}
	if s.Puller != nil && s.Sources != nil {
		sources, err := s.Sources(ctx)
		if err != nil {
			return Plan{}, fmt.Errorf("read the machines of the fleet: %w", err)
		}
		need.Sources = sources
	}
	return Decide(need)
}

func (s *Supply) copy(ctx context.Context, req EnsureRequest, plan Plan, out io.Writer) (*EnsureResult, error) {
	tr := TransferRequest{Release: req.Release.ID, Refs: plan.Refs, Arch: s.Images.Arch}
	stream, err := s.Puller.PullImages(ctx, plan.From, tr)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	head := bufio.NewReader(stream)
	if _, err := head.Peek(1); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("copy %s of release %d from %s: it sent nothing",
				plural(len(plan.Refs), "image"), req.Release.ID, plan.From)
		}
		return nil, fmt.Errorf("copy %s of release %d from %s: %w",
			plural(len(plan.Refs), "image"), req.Release.ID, plan.From, err)
	}
	moved, err := s.Images.Receive(ctx, tr, head)
	if err != nil {
		return nil, err
	}
	emit(out, progress.Event{
		App: req.App, Env: req.Env, Action: ActionBuild, Step: string(PlanCopy),
		Status: progress.StatusChanged,
		Detail: fmt.Sprintf("%s of release %s copied from %s: %s in %s",
			plural(len(plan.Refs), "image"), req.Release.Short(), plan.From,
			bytesOf(moved.Bytes), moved.Duration.Round(timeStep)),
	})
	rel := *req.Release
	if len(moved.Images) > 0 {
		rel.Built = append(append(Images(nil), rel.Built...), moved.Images...)
		rel.Built.Sort()
	}

	if s.Adopt != nil {
		adopted, err := s.Adopt(ctx, &rel)
		if err != nil {
			return nil, err
		}
		if adopted != nil {
			adopted.Built = rel.Built
			rel = *adopted
		}
	}
	return &EnsureResult{Release: &rel, Plan: plan, Copied: moved}, nil
}

func (s *Supply) build(ctx context.Context, req EnsureRequest, plan Plan, out io.Writer) (*EnsureResult, error) {
	res, err := s.Builder.Build(ctx, BuildRequest{
		App: req.App, Env: req.Env, Ref: req.Ref, Services: req.Services, Force: req.Force,
	}, out)
	if err != nil {
		return nil, err
	}
	plan.Action = PlanBuild
	if !res.Built {

		plan.Action, plan.Refs, plan.Services = PlanHave, nil, nil
	}
	return &EnsureResult{Release: res.Release, Plan: plan, Built: res.Built}, nil
}

func (s *Supply) machine() string {
	if s.Images == nil {
		return ""
	}
	return s.Images.Machine
}

const timeStep = 100 * time.Millisecond

func bytesOf(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

func emit(w io.Writer, e progress.Event) {
	if w == nil {
		return
	}

	_ = progress.Emit(w, e)
}

type Resolver interface {
	ResolveTree(ctx context.Context, app, env, ref string) (commit, tree string, err error)
}

func (s *Supply) lookup(ctx context.Context, req EnsureRequest, out io.Writer) *Release {
	if s.Lookup == nil {
		return nil
	}
	r, ok := s.Builder.(Resolver)
	if !ok {
		return nil
	}
	_, tree, err := r.ResolveTree(ctx, req.App, req.Env, req.Ref)
	if err != nil || tree == "" {
		return nil
	}
	rel, err := s.Lookup(ctx, req.App, tree)
	if err != nil {
		emit(out, progress.Event{
			App: req.App, Env: req.Env, Action: ActionBuild, Step: string(PlanCopy),
			Status: progress.StatusWarning,
			Detail: fmt.Sprintf("could not ask the fleet about release %s: %v", tree, err),
		})
		return nil
	}
	return rel
}

func (s *Supply) report(ctx context.Context, rel *Release, out io.Writer) {
	if s.Report == nil || rel == nil || s.Images == nil {
		return
	}
	held := make(Images, 0, len(rel.Images))
	for _, service := range rel.Services() {
		held = append(held, Image{
			ReleaseID: rel.ID, Service: service, Ref: rel.Images[service],
			Arch: s.Images.Arch, Machine: s.machine(), BuiltAt: rel.BuiltAt,
		})
	}
	if err := s.Report(ctx, rel, held); err != nil {
		emit(out, progress.Event{
			App: rel.App, Action: ActionBuild, Step: "index",
			Status: progress.StatusWarning,
			Detail: fmt.Sprintf("could not tell the fleet what this machine holds: %v", err),
		})
	}
}

func mergeImages(a, b Images) Images {
	if len(b) == 0 {
		return a
	}
	type key struct{ arch, service, machine string }
	out := make(Images, 0, len(a)+len(b))
	at := map[key]int{}
	for _, half := range []Images{a, b} {
		for _, im := range half {
			k := key{im.Arch, im.Service, im.Machine}
			if i, ok := at[k]; ok {
				if out[i].ID == "" {
					out[i] = im
				}
				continue
			}
			at[k] = len(out)
			out = append(out, im)
		}
	}
	out.Sort()
	return out
}
