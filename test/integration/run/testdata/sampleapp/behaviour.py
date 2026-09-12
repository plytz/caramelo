#!/usr/bin/env python3
"""What / answers.

Imported once at start, like version.py and health.py and for the same reason: a
commit whose / is broken must break the replicas started from it and no others.
This is the default — a working build.

The lab's `broken` commit copies behaviour.broken.py over this file, which is how
a deploy that has to be rolled back is made: the replicas start, they are
healthy, the smoke test passes, and then the first public request gets a 500.
That is exactly the failure a health check cannot catch and a watch window can.
"""

STATUS = 200
BODY = None
