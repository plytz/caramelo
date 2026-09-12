#!/usr/bin/env python3
"""A deploy.check that always refuses.

The `prodcheck` branch points deploy.check at this, so the lab can prove what a
failing gate costs: the deploy ends `failed` at `check`, no table was pushed, and
no client ever saw the new replicas. The exit code is 3 rather than 1 so that a
reader of the logs can tell this refusal from a crash.
"""

import sys

print("check: refusing this release on purpose (smoke_fail.py)", file=sys.stderr)
sys.exit(3)
