#!/usr/bin/env python3
"""deploy.check: the smoke test.

It runs as a one-off container of the new release, on the environment's network,
after every new replica is healthy and probed and *before* the flip. Nothing a
client can see has changed yet, so a non-zero exit here costs nothing but the
deploy.

It asks the two questions that matter at that moment:

  1. did `deploy.before` run for *this* release — the newest row in `migrations`
     names this version — so that a migration that silently did nothing cannot
     be promoted;
  2. does the *new* pool answer, and does it identify itself as this release.

     The URL comes from CARAMELO_CHECK_URL, which the deploy sets to one of the
     replicas it has just started, by container name. It is deliberately not the
     service's network alias (http://web:<port>/, which caramelo.yaml offers as
     SMOKE_URL for an app that wants it): Docker round-robins that alias between
     the old pool and the new one, so half the time a check against it would be
     testing the build that is already serving and passing for the wrong reason.

     It asks /healthz and not /, and that is the lab's whole fourth case. A smoke
     test that asked / would catch the broken commit here, at the gate, with
     nothing flipped — which is a fine thing for a deploy to do and the wrong
     thing for a test of the *watch* to rely on. The build that has to reach the
     watch is one that is healthy, identifies itself, passes every gate, and then
     answers 500 to the public; asking the health endpoint is what a check can
     honestly assert before any client has been let near it.

SMOKE_FAIL=1 makes it fail on purpose, which is how the lab proves a failing
check stops a deploy before anything is flipped.
"""

import os
import sys
import urllib.request

import pgwire
import version


def check_migration():
    with pgwire.connect() as conn:
        latest = conn.scalar("SELECT version FROM migrations ORDER BY id DESC LIMIT 1")
    if latest is None:
        return "the migrations table is empty: deploy.before did not run"
    if latest != version.VERSION:
        return "the newest migration is %r, want this release's %r" % (latest, version.VERSION)
    print("smoke: the migration for %s is in the database" % version.VERSION)
    return None


def check_pool():
    url = os.environ.get("CARAMELO_CHECK_URL") or os.environ.get("SMOKE_URL")
    if not url:
        print("smoke: no CARAMELO_CHECK_URL, so the new pool was not asked anything")
        return None
    url = url.rstrip("/") + "/healthz"
    try:
        with urllib.request.urlopen(url, timeout=10) as res:
            status, body = res.status, res.read().decode("utf-8", "replace")
    except Exception as err:
        return "GET %s failed: %s" % (url, err)
    if status != 200:
        return "GET %s answered %d" % (url, status)
    if "ok" not in body:
        return "GET %s answered %r, want it to be well" % (url, body)
    print("smoke: %s answers well, so the new pool is up" % url)
    return None


def main():
    if os.environ.get("SMOKE_FAIL"):
        print("smoke: SMOKE_FAIL is set, so this check fails on purpose", file=sys.stderr)
        return 3
    for problem in (check_migration(), check_pool()):
        if problem:
            print("smoke: %s" % problem, file=sys.stderr)
            return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
