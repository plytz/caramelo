#!/usr/bin/env python3
"""greeting.py for the branches whose greeting comes from the vault.

The release branches take what the app says from the GREETING *secret* rather
than from a line in the source, which is how the lab proves three things at
once: that a secret reaches a container's environment, that the narrowest scope
wins (an environment's GREETING over the app's), and that changing a secret is a
change the next deploy rolls out.

A missing secret is said out loud rather than defaulted, because an app that
quietly serves "None" for a missing API key is the failure mode the vault exists
to prevent.
"""

import os


def greeting():
    value = os.environ.get("GREETING")
    if not value:
        return "no GREETING secret in %s\n" % os.environ.get("CARAMELO_ENV", "unknown")
    return "%s %s\n" % (value, os.environ.get("CARAMELO_ENV", "unknown"))
