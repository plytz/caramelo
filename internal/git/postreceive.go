package git

import (
	"context"
	"fmt"
	"path/filepath"
)

const (
	PostReceiveHookName = "hooks/post-receive"

	PushRecordEnv = "CARAMELO_PUSH_RECORD"

	PushRecordDir = "caramelo-push"
)

const PostReceiveHook = `#!/bin/sh
# Installed and kept up to date by caramelo. Edits are overwritten.
#
# Writes down what this push carried so caramelod can record it: the branches
# that actually moved, and the push options the client sent with -o. The daemon
# names the file in CARAMELO_PUSH_RECORD and reads it once receive-pack is done,
# because only the daemon knows which peer's key opened the connection.
#
# Nothing here may fail the push or say anything to the client: git ignores what
# post-receive exits with, everything this writes to stdout or stderr would
# reach the developer's terminal as a "remote:" line, and a push that landed has
# landed whether or not we managed to write it down. Hence no set -e, every
# write redirected, and one unconditional exit 0.
if [ -z "$CARAMELO_PUSH_RECORD" ]; then
	exit 0
fi

{
	i=0
	count="${GIT_PUSH_OPTION_COUNT:-0}"
	while [ "$i" -lt "$count" ]; do
		# A push option is whatever the client typed, newlines included, so the
		# CR and LF go before the value is written: a forged second line here
		# would otherwise read back as a ref this push never carried.
		value=$(eval printf '%s' "\"\${GIT_PUSH_OPTION_$i}\"" | tr -d '\r\n')
		printf 'option %s\n' "$value"
		i=$((i + 1))
	done
	while read -r old new ref; do
		printf 'ref %s %s %s\n' "$old" "$new" "$ref"
	done
} 2>/dev/null >> "$CARAMELO_PUSH_RECORD"

exit 0
`

type PostReceiver interface {
	EnsurePostReceive(ctx context.Context, repo string) (bool, error)
}

var _ PostReceiver = (*CLI)(nil)

func (c *CLI) EnsurePostReceive(ctx context.Context, repo string) (bool, error) {
	if repo == "" {
		return false, fmt.Errorf("install the post-receive hook: no repository given")
	}
	drop := filepath.Join(repo, PushRecordDir)
	if res, err := c.command(ctx, "install", "-d", "-m", "0700", drop); err != nil {
		return false, err
	} else if res.ExitCode != 0 {
		return false, cmdErr([]string{"install", "-d", drop}, res)
	}
	return c.ensureHook(ctx, repo, PostReceiveHookName, PostReceiveHook)
}
