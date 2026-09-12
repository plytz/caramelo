package setup

import (
	"context"
	"fmt"
	"strings"

	"github.com/plytz/caramelo/internal/fleet"
	"github.com/plytz/caramelo/internal/runner"
	"github.com/plytz/caramelo/internal/serverconfig"
)

const JoinStepName = "join"

const tokenFromStdin = "-"

type JoinStep struct{}

func NewJoinStep() *JoinStep { return &JoinStep{} }

func (s *JoinStep) Name() string { return JoinStepName }

func (s *JoinStep) Check(ctx context.Context, env *Env) (bool, string, error) {
	spec := env.Opts.Join
	if spec.Empty() {
		return false, "", Skip{Reason: "no --join-token"}
	}
	ticket, err := spec.Ticket()
	if err != nil {
		return false, "", err
	}
	cfg := env.Config
	if cfg.IsMember() && cfg.Fleet.Hub.Name == ticket.Hub && cfg.Fleet.Hub.PublicKey == ticket.PublicKey {
		return true, fmt.Sprintf("a member of %s at %s", ticket.Hub, ticket.Endpoint), nil
	}
	if cfg.IsMember() {

		return false, "", fmt.Errorf(
			"this machine is already a member of %s; remove it there first (`caramelo machine remove %s`) before joining %s",
			cfg.Fleet.Hub.Name, cfg.Fleet.Hub.Name, ticket.Hub)
	}
	return false, fmt.Sprintf("not a member of %s yet", ticket.Hub), nil
}

func (s *JoinStep) Apply(ctx context.Context, env *Env) error {
	spec := env.Opts.Join
	ticket, err := spec.Ticket()
	if err != nil {
		return err
	}

	args := []string{"machine", "join", ticket.Endpoint, "--token", tokenFromStdin, "--json"}
	if env.ConfigDir != "" && env.ConfigDir != serverconfig.DefaultConfigDir {
		args = append(args, "--config-dir", env.ConfigDir)
	}
	if spec.Name != "" {
		args = append(args, "--name", spec.Name)
	}
	if env.Config.Fleet.Private {
		args = append(args, "--private")
	}
	res, err := runCmd(ctx, env, runner.Cmd{
		Name: serverconfig.BinaryPath, Args: args, Stdin: strings.NewReader(spec.Token),
	})
	if err != nil {
		return fmt.Errorf("join %s at %s: %w", ticket.Hub, ticket.Endpoint, err)
	}
	if res.ExitCode != 0 {

		return fmt.Errorf("join %s at %s: exit %d: %s",
			ticket.Hub, ticket.Endpoint, res.ExitCode, firstLine(res.Stderr, res.Stdout))
	}
	logf(env, "joined %s at %s", ticket.Hub, ticket.Endpoint)
	return nil
}

type JoinSpec struct {
	Token string `json:"token,omitempty"`

	Name string `json:"name,omitempty"`
}

func (j JoinSpec) Empty() bool { return strings.TrimSpace(j.Token) == "" }

func (j JoinSpec) Ticket() (fleet.Ticket, error) {
	return fleet.ParseTicket(j.Token)
}

func (j JoinSpec) Redacted() JoinSpec {
	if j.Token != "" {
		j.Token = "…"
	}
	return j
}
