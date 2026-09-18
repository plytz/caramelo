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
	if cfg.IsMember() && cfg.Member.Hub.PublicKey == ticket.PublicKey && cfg.FleetName() == ticket.Fleet {
		return true, fmt.Sprintf("a member of the fleet %s at %s", ticket.Fleet, ticket.Endpoint), nil
	}
	if cfg.IsMember() {

		return false, "", fmt.Errorf(
			"this machine is already a member of the fleet %s; leave it first (`caramelo member leave` here, "+
				"`caramelo member remove %s` on its hub) before joining %s",
			cfg.FleetName(), cfg.Name, ticket.Fleet)
	}
	return false, fmt.Sprintf("not a member of the fleet %s yet", ticket.Fleet), nil
}

func (s *JoinStep) Apply(ctx context.Context, env *Env) error {
	spec := env.Opts.Join
	ticket, err := spec.Ticket()
	if err != nil {
		return err
	}

	args := []string{"member", "join", ticket.Endpoint, "--token", tokenFromStdin, "--json"}
	if env.ConfigDir != "" && env.ConfigDir != serverconfig.DefaultConfigDir {
		args = append(args, "--config-dir", env.ConfigDir)
	}
	if spec.Name != "" {
		args = append(args, "--name", spec.Name)
	}
	if env.PrivateDoor() {
		args = append(args, "--private")
	}
	res, err := runCmd(ctx, env, runner.Cmd{
		Name: serverconfig.BinaryPath, Args: args, Stdin: strings.NewReader(spec.Token),
	})
	if err != nil {
		return fmt.Errorf("join the fleet %s at %s: %w", ticket.Fleet, ticket.Endpoint, err)
	}
	if res.ExitCode != 0 {

		return fmt.Errorf("join the fleet %s at %s: exit %d: %s",
			ticket.Fleet, ticket.Endpoint, res.ExitCode, firstLine(res.Stderr, res.Stdout))
	}
	logf(env, "joined the fleet %s at %s", ticket.Fleet, ticket.Endpoint)
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
