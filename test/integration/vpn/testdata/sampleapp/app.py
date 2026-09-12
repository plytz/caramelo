#!/usr/bin/env python3
"""The sample app the `run` lab suite drives.

Deliberately stdlib-only and tiny: what M4 has to get right is the container
around the code, not the code. Three routes, each answering a question the
suite asks:

  /         the environment's own name, so two environments can be told apart
  /healthz  the service's health check (caramelo.yaml points `health:` at it)
  /deps     a TCP connection to every dependency by its *network* name, which
            is the whole point of the per-environment Docker network

greeting.py is reloaded on every request — the smallest version of the
reloader a real dev server has — so an edit to the environment's worktree
shows up in the next response with no rebuild and no restart.

It listens on $PORT, which Caramelo sets to the environment's block port.
"""

import importlib
import os
import socket
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import greeting

DEPS = (("db", 5432), ("cache", 6379))


def probe(host, port):
    """Report whether a TCP connection to host:port opens."""
    try:
        with socket.create_connection((host, port), timeout=5):
            return "ok"
    except OSError as err:
        return "error: %s" % (err,)


def deps_report():
    return "".join("%s %s\n" % (name, probe(name, port)) for name, port in DEPS)


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path.startswith("/healthz"):
            body = "ok\n"
        elif self.path.startswith("/deps"):
            body = deps_report()
        else:
            body = importlib.reload(greeting).greeting()
        payload = body.encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, fmt, *args):
        print("request %s" % (fmt % args), flush=True)


def main():
    port = int(os.environ.get("PORT", "8000"))
    print("sampleapp: listening on 0.0.0.0:%d" % port, flush=True)
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
