#!/usr/bin/env python3
"""deploy.before: the migration.

It runs as a one-off container of the *new* release, on the environment's own
network, before any new replica starts — which is the whole point of the hook: a
new replica must never meet an old schema.

What it does is the smallest thing that is observably a migration: it creates a
table if it is not there and writes one row naming the release's version. The
lab asserts the row exists, `deploy.check` reads it back, and a deploy that got
as far as `before` can therefore be told apart from one that did not.

It connects with DATABASE_URL, whose password is the environment's DB_PASSWORD
secret — so this script failing is also the proof that the dependency took the
password the user chose.
"""

import sys

import pgwire
import version


def main():
    try:
        conn = pgwire.connect()
    except Exception as err:
        print("migrate: cannot reach the database: %s" % err, file=sys.stderr)
        return 1
    with conn:
        conn.execute(
            "CREATE TABLE IF NOT EXISTS migrations ("
            "id serial PRIMARY KEY, "
            "version text NOT NULL, "
            "at timestamptz NOT NULL DEFAULT now())"
        )
        conn.execute(
            "INSERT INTO migrations (version) VALUES ('%s')" % version.VERSION
        )
        count = conn.scalar("SELECT count(*) FROM migrations", "0")
    print("migrate: version %s written, %s row(s) in migrations" % (version.VERSION, count))
    return 0


if __name__ == "__main__":
    sys.exit(main())
