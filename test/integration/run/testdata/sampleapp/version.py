#!/usr/bin/env python3
"""What build this container is running.

app.py imports this **once, at start**, and never reloads it. That is the
whole point: every replica of a service shares one bind-mounted worktree, so
anything read per request changes for the old replicas and the new ones at the
same instant, and a rolling replacement would be invisible. A module imported
at start belongs to the container, so the answer changes only when a container
does — which is exactly what the edge suite's load generator watches for.

The suite rewrites this line, commits, and runs `caramelo up`.
"""

VERSION = "v1"
