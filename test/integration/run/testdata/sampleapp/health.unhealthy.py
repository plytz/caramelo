#!/usr/bin/env python3
"""The health check of a commit that must never reach the pool.

Copied over health.py by the edge suite: `caramelo up` starts the first new
replica, its health check never passes, the rollout stops there, the old
replicas keep serving, nothing is ever drained and the load generator sees
nothing at all.
"""

STATUS = 500
BODY = "not ready\n"
