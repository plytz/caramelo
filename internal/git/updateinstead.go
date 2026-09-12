package git

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/plytz/caramelo/internal/runner"
)

const (
	DenyCurrentBranchKey = "receive.denyCurrentBranch"

	UpdateInstead = "updateInstead"

	PushToCheckoutHookName = "hooks/push-to-checkout"
)

const PushToCheckoutHook = `#!/bin/sh
# Installed and kept up to date by caramelo. Edits are overwritten.
#
# git runs this when a push lands on a branch that one of this repository's
# worktrees has checked out (receive.denyCurrentBranch = updateInstead): the
# environment's checkout is updated in place, unless somebody is working in
# it, in which case the push is refused and their work is left alone.
#
# GIT_DIR and GIT_WORK_TREE name the worktree; $1 is the commit being pushed.
set -e
worktree="${GIT_WORK_TREE:-$PWD}"
advice="commit or discard them in the environment (caramelo env exec ENV -- git status), then push again"

git update-index -q --refresh || true
if ! git diff-files --quiet --ignore-submodules; then
	echo "caramelo: refusing to update $worktree: it has uncommitted changes" >&2
	echo "caramelo: $advice" >&2
	exit 1
fi
if ! git diff-index --quiet --cached --ignore-submodules HEAD --; then
	echo "caramelo: refusing to update $worktree: it has staged changes" >&2
	echo "caramelo: $advice" >&2
	exit 1
fi
git read-tree -u -m "$1"
`

type PushUpdater interface {
	EnsureUpdateInstead(ctx context.Context, repo string) (bool, error)
}

var _ PushUpdater = (*CLI)(nil)

func (c *CLI) EnsureUpdateInstead(ctx context.Context, repo string) (bool, error) {
	if repo == "" {
		return false, errors.New("git config " + DenyCurrentBranchKey + ": no repository given")
	}
	config, err := c.ensureDenyCurrentBranch(ctx, repo)
	if err != nil {
		return false, err
	}
	hook, err := c.ensurePushToCheckout(ctx, repo)
	if err != nil {
		return false, err
	}
	return config || hook, nil
}

func (c *CLI) ensureDenyCurrentBranch(ctx context.Context, repo string) (bool, error) {

	args := c.in(repo, "config", "--local", "--get", DenyCurrentBranchKey)
	res, err := c.run(ctx, args...)
	if err != nil {
		return false, err
	}
	switch res.ExitCode {
	case 0:

		if strings.EqualFold(strings.TrimSpace(res.Stdout), UpdateInstead) {
			return false, nil
		}
	case 1:
	default:
		return false, cmdErr(args, res)
	}
	if _, err := c.runOK(ctx, c.in(repo, "config", "--local", DenyCurrentBranchKey, UpdateInstead)...); err != nil {
		return false, err
	}
	return true, nil
}

func (c *CLI) ensurePushToCheckout(ctx context.Context, repo string) (bool, error) {
	return c.ensureHook(ctx, repo, PushToCheckoutHookName, PushToCheckoutHook)
}

func (c *CLI) ensureHook(ctx context.Context, repo, name, body string) (bool, error) {
	path := filepath.Join(repo, filepath.FromSlash(name))
	res, err := c.command(ctx, "cat", path)
	if err != nil {
		return false, err
	}
	if res.ExitCode == 0 && res.Stdout == body {
		return false, nil
	}

	if res, err := c.command(ctx, "install", "-d", "-m", "0750", filepath.Dir(path)); err != nil {
		return false, err
	} else if res.ExitCode != 0 {
		return false, cmdErr([]string{"install", "-d", filepath.Dir(path)}, res)
	}

	if c.Runner == nil {
		return false, fmt.Errorf("write %s: no command runner configured", path)
	}
	res, err = c.Runner.Run(ctx, runner.Cmd{
		Name:  "tee",
		Args:  []string{"--", path},
		User:  c.User,
		Stdin: strings.NewReader(body),
	})
	if err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	if res.ExitCode != 0 {
		return false, cmdErr([]string{"tee", "--", path}, res)
	}

	if res, err := c.command(ctx, "chmod", "0755", path); err != nil {
		return false, err
	} else if res.ExitCode != 0 {
		return false, cmdErr([]string{"chmod", "0755", path}, res)
	}
	return true, nil
}

func hookPath(repo string) string {
	return filepath.Join(repo, filepath.FromSlash(PushToCheckoutHookName))
}

func (c *CLI) command(ctx context.Context, name string, args ...string) (runner.Result, error) {
	if c.Runner == nil {
		return runner.Result{}, fmt.Errorf("%s %s: no command runner configured", name, strings.Join(args, " "))
	}
	res, err := c.Runner.Run(ctx, runner.Cmd{Name: name, Args: args, User: c.User})
	if err != nil {
		return res, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return res, nil
}

func FixUpUpdateInstead(ctx context.Context, u PushUpdater, repos []string) (int, error) {
	if u == nil {
		return 0, errors.New("git config " + DenyCurrentBranchKey + ": no repository driver")
	}
	var (
		fixed int
		errs  []error
	)
	for _, repo := range repos {
		changed, err := u.EnsureUpdateInstead(ctx, repo)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", repo, err))
			continue
		}
		if changed {
			fixed++
		}
	}
	return fixed, errors.Join(errs...)
}
