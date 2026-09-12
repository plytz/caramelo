#!/usr/bin/env python3
"""A service that allocates until the kernel stops it.

It is the `oom` branch's only service, and it exists to prove one sentence of
the milestone: a `resources.memory` becomes a real `--memory` on the container,
an allocation past it is an OOM kill, Docker restarts the container, and
caramelod notices and writes an event.

It sleeps first, for two reasons: `caramelo up` must be able to finish (a
container that is already being killed while up is waiting would look like a
failed start rather than a supervision story), and the restart count has to
climb *after* the environment is up for the health loop to call it a crash loop.

Nothing about this is subtle: 8 MiB at a time, held in a list so nothing can be
collected, with a line on stdout each time so `caramelo logs` shows how far it
got before the kernel intervened.
"""

import os
import sys
import time

CHUNK = 8 * 1024 * 1024
DELAY = float(os.environ.get("HOG_DELAY") or 20)
LIMIT_CHUNKS = int(os.environ.get("HOG_CHUNKS") or 512)


def main():
    print("hog: up, allocating in %.0fs" % DELAY, flush=True)
    time.sleep(DELAY)
    held = []
    for i in range(LIMIT_CHUNKS):
        block = bytearray(CHUNK)
        for offset in range(0, CHUNK, 4096):
            block[offset] = 1
        held.append(block)
        print("hog: %d MiB" % ((i + 1) * CHUNK // (1024 * 1024)), flush=True)
        time.sleep(0.05)
    print("hog: nothing stopped me, which means the limit is not being applied",
          file=sys.stderr, flush=True)
    return 1


if __name__ == "__main__":
    sys.exit(main())
