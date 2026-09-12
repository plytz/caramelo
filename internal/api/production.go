package api

import (
	"context"
	"io"

	"github.com/plytz/caramelo/internal/env"
	"github.com/plytz/caramelo/internal/release"
	"github.com/plytz/caramelo/internal/vault"
)

type ProductionService interface {
	Build(ctx context.Context, req release.BuildRequest, progress io.Writer) (*BuildResult, error)

	Deploy(ctx context.Context, req env.DeployRequest, progress io.Writer) (*DeployResult, error)

	Promote(ctx context.Context, req env.PromoteRequest, progress io.Writer) (*DeployResult, error)

	Rollback(ctx context.Context, req env.RollbackRequest, progress io.Writer) (*DeployResult, error)

	Releases(ctx context.Context, app, name string, limit int) (*ReleasesResult, error)

	VaultSet(ctx context.Context, req VaultSetRequest) (*VaultResult, error)

	VaultList(ctx context.Context, req VaultListRequest) (*VaultResult, error)

	VaultRemove(ctx context.Context, req VaultRemoveRequest) (*VaultResult, error)

	VaultExport(ctx context.Context, req VaultExportRequest) (*VaultExportResult, error)

	Events(ctx context.Context, req env.EventsRequest, out io.Writer, follow bool) error
}

type BuildResult struct {
	App string `json:"app"`
	Env string `json:"env"`

	Release *release.Release `json:"release"`

	Built bool `json:"built"`

	Images []string `json:"images,omitempty"`
}

type DeployResult struct {
	Deploy *env.Deploy `json:"deploy"`

	Env *env.Env `json:"env,omitempty"`
}

type ReleasesResult struct {
	App string `json:"app"`
	Env string `json:"env"`

	Deploys []env.Deploy `json:"deploys"`

	Current *release.Release `json:"current,omitempty"`

	Releases []release.Release `json:"releases,omitempty"`
}

type VaultSetRequest struct {
	Scope vault.Scope `json:"scope"`
	App   string      `json:"app,omitempty"`
	Env   string      `json:"env,omitempty"`

	Values map[string]string `json:"values"`
}

type VaultListRequest struct {
	Scope vault.Scope `json:"scope,omitempty"`
	App   string      `json:"app,omitempty"`
	Env   string      `json:"env,omitempty"`
}

type VaultRemoveRequest struct {
	Scope vault.Scope `json:"scope"`
	App   string      `json:"app,omitempty"`
	Env   string      `json:"env,omitempty"`
	Names []string    `json:"names"`
}

type VaultExportRequest struct {
	App string `json:"app"`
	Env string `json:"env"`

	Reveal bool `json:"reveal,omitempty"`

	Format string `json:"format,omitempty"`
}

type VaultResult struct {
	App string `json:"app,omitempty"`
	Env string `json:"env,omitempty"`

	Entries []vault.Entry `json:"entries"`

	Resolved map[string]vault.Scope `json:"resolved,omitempty"`

	Changed []string `json:"changed,omitempty"`

	Warnings []string `json:"warnings,omitempty"`
}

type VaultExportResult struct {
	App string `json:"app"`
	Env string `json:"env"`

	Values map[string]string `json:"values"`

	Sources map[string]vault.Scope `json:"sources,omitempty"`

	Revealed bool `json:"revealed"`

	Text string `json:"text,omitempty"`
}
