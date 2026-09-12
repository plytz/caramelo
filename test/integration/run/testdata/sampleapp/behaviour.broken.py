#!/usr/bin/env python3
"""What / answers on the lab's `broken` commit: an error, every time.

/healthz still answers 200, which is the point. A build that is healthy and
answers 500 to the world is what the watch window exists for, and what
deploy.max_errors measures.
"""

STATUS = 500
BODY = "the broken build\n"
