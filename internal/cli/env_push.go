package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/plytz/caramelo/internal/remote"
	"github.com/plytz/caramelo/internal/vpnclient"
)

type pushPlan struct {
	Dir string

	Remote string

	Ref string

	SourceBranch string

	Force bool

	Tunnel bool
}

func (p pushPlan) String() string {
	force := ""
	if p.Force {
		force = " (forced)"
	}
	return fmt.Sprintf("push %s to %s%s", p.Ref, p.Remote, force)
}

func (e *envCmd) pushBeforeCreate(cmd *cobra.Command) error {
	t, err := resolveTransport(cmd.Context(), e.a)
	if err != nil {

		return nil
	}
	plan, err := e.planPush(cmd.Context(), t)
	if err != nil || plan == nil {
		return err
	}
	e.a.printer().Infof("%s", plan)
	if plan.Tunnel {

		vpnclient.CloseTunnels()
	}
	return gitPush(cmd.Context(), *plan, e.a.stderr)
}

func (e *envCmd) planPush(ctx context.Context, t transport) (*pushPlan, error) {
	if e.create.noPush || (t.kind != kindSSH && t.kind != kindTunnel) {
		return nil, nil
	}
	if _, err := runGit(ctx, ".", "rev-parse", "--git-dir"); err != nil {
		return nil, nil
	}

	ref := strings.TrimSpace(e.create.from)
	if ref == "" {
		ref = currentBranch(ctx)
	}
	if ref == "" {
		return nil, &usageError{errors.New(
			"this checkout is not on a branch, so there is nothing to push by default: pass --from REF or --no-push")}
	}
	return &pushPlan{
		Dir:          ".",
		Remote:       pushURL(t.target, e.app),
		Ref:          ref,
		SourceBranch: pushSource(ctx, strings.TrimSpace(e.create.from)),
		Force:        e.create.force,
		Tunnel:       t.kind == kindTunnel,
	}, nil
}

func pushSource(ctx context.Context, from string) string {
	if from == "" {
		return currentBranch(ctx)
	}
	if _, err := runGit(ctx, ".", "rev-parse", "--verify", "--quiet", "refs/heads/"+from); err != nil {
		return ""
	}
	return from
}

func pushURL(target remote.Target, app string) string {
	host := target.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("ssh://%s@%s:%d/%s", target.User, host, target.Port, app)
}

var gitPush = runGitPush

func pushEnv(p pushPlan) ([]string, error) {
	if p.Tunnel {
		return gitSSHEnvTunnel(thisBinary()), nil
	}
	cmd, err := gitSSHCommand()
	if err != nil {
		return nil, err
	}
	return []string{"GIT_SSH_COMMAND=" + cmd}, nil
}

func gitSSHCommand() (string, error) {
	bin, args, err := sshInvocation()
	if err != nil {
		return "", err
	}

	return strings.Join(append([]string{remote.Quote(bin)}, remote.QuoteArgs(args)...), " "), nil
}

func pushArgs(p pushPlan) []string {
	args := []string{"-C", p.Dir, "push"}
	if p.Force {
		args = append(args, "--force")
	}
	if p.SourceBranch != "" {
		args = append(args, "-o", "caramelo.branch="+p.SourceBranch)
	}
	return append(args, p.Remote, p.Ref)
}

func runGitPush(ctx context.Context, p pushPlan, stderr io.Writer) error {
	gitEnv, err := pushEnv(p)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", pushArgs(p)...)
	cmd.Env = append(os.Environ(), gitEnv...)

	cmd.Stdout = stderr
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("git push %s to %s failed (exit %d); "+
				"a rejected push usually means the branch has moved on the machine — fetch it, or pass --force",
				p.Ref, p.Remote, ee.ExitCode())
		}
		return fmt.Errorf("run git push: %w", err)
	}
	return nil
}
