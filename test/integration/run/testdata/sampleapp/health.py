#!/usr/bin/env python3
"""What /healthz answers.

Imported once at start, like version.py and for the same reason: a commit
whose health check fails must fail the replicas started from it and no others.
If this were reloaded per request, pushing such a commit would make the
replicas that are already running unhealthy too — the shared worktree again —
and a rollout that has to stop harmlessly at its first new replica would look
like an outage instead.
"""

STATUS = 200
BODY = "ok\n"
