package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/config"
	"github.com/plytz/caramelo/internal/state"
)

const NamePrefix = "caramelo"

const TreeLen = 12

func ImageRef(app, service, tree string) string {
	return NamePrefix + "/" + app + "/" + service + ":" + ShortTree(tree)
}

func ShortTree(tree string) string {
	tree = strings.TrimSpace(tree)
	if len(tree) > TreeLen {
		return tree[:TreeLen]
	}
	return tree
}

const (
	LabelApp     = "caramelo.app"
	LabelService = "caramelo.service"
	LabelTree    = "caramelo.tree"
	LabelVersion = "caramelo.version"
)

func ImageLabels(app, service, tree, version string) map[string]string {
	l := map[string]string{LabelApp: app}
	if service != "" {
		l[LabelService] = service
	}
	if tree != "" {
		l[LabelTree] = ShortTree(tree)
	}
	if version != "" {
		l[LabelVersion] = version
	}
	return l
}

type Release struct {
	ID  int64  `json:"id"`
	App string `json:"app"`

	Commit string `json:"commit"`

	Tree string `json:"tree"`

	Ref string `json:"ref,omitempty"`

	Images map[string]string `json:"images,omitempty"`

	Config *config.App `json:"config,omitempty"`

	BuiltBy string    `json:"built_by,omitempty"`
	BuiltAt time.Time `json:"built_at"`

	Machine string `json:"machine,omitempty"`

	Built Images `json:"built,omitempty"`
}

func (r Release) Arches() []string { return r.Built.Arches() }

func (r Release) Image(service string) (string, bool) {
	ref, ok := r.Images[service]
	return ref, ok
}

func (r Release) Services() []string {
	out := make([]string, 0, len(r.Images))
	for s := range r.Images {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func (r Release) ImageList() []string {
	out := make([]string, 0, len(r.Images))
	for _, s := range r.Services() {
		out = append(out, r.Images[s])
	}
	return out
}

func (r Release) Short() string {
	if r.Commit == "" {
		return ShortTree(r.Tree)
	}
	commit := r.Commit
	if len(commit) > TreeLen {
		commit = commit[:TreeLen]
	}
	return ShortTree(r.Tree) + " (" + commit + ")"
}

func FromRecord(r *state.Release) (*Release, error) {
	if r == nil {
		return nil, errors.New("read a release: no row")
	}
	out := &Release{
		ID: r.ID, App: r.App, Commit: r.Commit, Tree: r.Tree, Ref: r.Ref,
		BuiltBy: r.BuiltBy, BuiltAt: r.BuiltAt,

		Machine: r.Machine,
	}
	if r.ImagesJSON != "" {
		if err := json.Unmarshal([]byte(r.ImagesJSON), &out.Images); err != nil {
			return nil, fmt.Errorf("release %s of app %q: decode its images: %w", ShortTree(r.Tree), r.App, err)
		}
	}
	if r.ConfigJSON != "" {
		cfg := &config.App{}
		if err := json.Unmarshal([]byte(r.ConfigJSON), cfg); err != nil {
			return nil, fmt.Errorf("release %s of app %q: decode its %s: %w",
				ShortTree(r.Tree), r.App, config.FileName, err)
		}
		out.Config = cfg
	}
	return out, nil
}

type BuildRequest struct {
	App string `json:"app"`
	Env string `json:"env"`

	Ref string `json:"ref,omitempty"`

	Services []string `json:"services,omitempty"`

	Force bool `json:"force,omitempty"`
}

type BuildResult struct {
	Release *Release `json:"release"`

	Built bool `json:"built"`

	Images []string `json:"images,omitempty"`

	Plan Plan `json:"plan,omitzero"`
}

type Builder interface {
	Build(ctx context.Context, req BuildRequest, progress io.Writer) (*BuildResult, error)

	Prune(ctx context.Context, app string, keep int) ([]string, error)
}

var ErrNotImplemented = errors.New("release: not implemented")

type NotImplemented struct{}

func (NotImplemented) Build(_ context.Context, req BuildRequest, _ io.Writer) (*BuildResult, error) {
	return nil, fmt.Errorf("build a release of env %q of app %q: %w", req.Env, req.App, ErrNotImplemented)
}

func (NotImplemented) Prune(_ context.Context, app string, keep int) ([]string, error) {
	return nil, fmt.Errorf("prune the releases of app %q past %d: %w", app, keep, ErrNotImplemented)
}
