package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
	"github.com/plytz/caramelo/internal/vpn"
)

type Status string

const (
	StatusOK          Status = "ok"
	StatusChanged     Status = "changed"
	StatusWouldChange Status = "would-change"
	StatusSkipped     Status = "skipped"
	StatusFailed      Status = "failed"
)

type Env struct {
	Config serverconfig.Config

	ConfigDir string

	Opts Options
	Run  runner.Runner

	Log     io.Writer
	DryRun  bool
	Version string

	BinaryPath string

	ConfigChanged bool
}

type Options struct {
	AuthorizedKeysFile string
	Yes                bool
	Force              bool
	LowPorts           bool
	InstallPackages    bool

	Join JoinSpec

	Peer PeerSpec
}

type PeerSpec struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

func (p PeerSpec) Empty() bool { return p.Name == "" && p.PublicKey == "" }

func (p PeerSpec) String() string {
	if p.Empty() {
		return ""
	}
	return p.Name + " " + p.PublicKey
}

func (p PeerSpec) Validate() error {
	switch {
	case p.Name == "":
		return errors.New("--peer: no name")
	case p.PublicKey == "":
		return fmt.Errorf("--peer %s: no public key", p.Name)
	}
	key, err := vpn.ParseKey(p.PublicKey)
	if err != nil {
		return fmt.Errorf("--peer %s: %w", p.Name, err)
	}
	if key.IsZero() {
		return fmt.Errorf("--peer %s: the public key is empty", p.Name)
	}
	return nil
}

func ParsePeer(s string) (PeerSpec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return PeerSpec{}, nil
	}
	i := strings.IndexAny(s, " \t=,")
	if i < 0 {
		return PeerSpec{}, fmt.Errorf("--peer %q: want 'NAME KEY' (the peer's name and its WireGuard public key)", s)
	}
	p := PeerSpec{
		Name:      strings.TrimSpace(s[:i]),
		PublicKey: strings.TrimSpace(s[i+1:]),
	}
	if err := p.Validate(); err != nil {
		return PeerSpec{}, err
	}
	return p, nil
}

type Step interface {
	Name() string

	Check(ctx context.Context, env *Env) (done bool, detail string, err error)

	Apply(ctx context.Context, env *Env) error
}

type Result struct {
	Step     string        `json:"step"`
	Status   Status        `json:"status"`
	Detail   string        `json:"detail,omitempty"`
	Error    string        `json:"error,omitempty"`
	Duration time.Duration `json:"duration"`
}

type Report struct {
	RunID   string   `json:"run_id"`
	Version string   `json:"version"`
	DryRun  bool     `json:"dry_run"`
	Results []Result `json:"results"`
	Changed int      `json:"changed"`
	Failed  int      `json:"failed"`
}

type Skip struct{ Reason string }

func (s Skip) Error() string { return "skipped: " + s.Reason }
