#!/usr/bin/env python3
"""What the app says, in its own module so that it can be reloaded.

app.py re-imports this on every request — the tiny version of the reloader a
real dev server has. That is what makes an edit visible without a rebuild and
without a restart: the container runs the environment's worktree, mounted, and
the worktree is what `git push` updates.

The lab suite rewrites the MESSAGE line, commits, runs `caramelo up` and
expects the next response to have changed.
"""

import os

MESSAGE = "hello from"


def greeting():
    return "%s %s\n" % (MESSAGE, os.environ.get("CARAMELO_ENV", "unknown"))
