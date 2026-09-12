#!/usr/bin/env python3
"""The failure path: a service that dies immediately.

`caramelo up` must give up at its timeout, put this program's last output on
stderr and leave the container in place so `logs` and `env show` can say why.
"""

import sys

print("sampleapp: starting up", flush=True)
print("sampleapp: nothing to run here, giving up", file=sys.stderr, flush=True)
sys.exit(1)
