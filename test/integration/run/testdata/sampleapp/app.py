#!/usr/bin/env python3
"""The sample app the `run` and `edge` lab suites drive.

Deliberately stdlib-only and tiny: what M4 and M6 have to get right is the
container around the code and the door in front of it, not the code. Six
routes, each answering a question a suite asks:

  /         the environment's name, the build and which replica answered
  /healthz  the service's health check (caramelo.yaml points `health:` at it)
  /deps     a TCP connection to every dependency by its *network* name, which
            is the whole point of the per-environment Docker network
  /slow     a response that takes SLOW_SECONDS, so a rollout can be caught
            with a request still in flight on the replica it is draining
  /ws       a WebSocket echo, so an upgrade can be proved to pass through the
            edge — and to be held open until the drain deadline, not before

Two of the modules it imports are treated differently on purpose:

  greeting  reloaded on *every request* — the smallest version of the reloader
            a real dev server has, so an edit to the worktree shows up with no
            rebuild and no restart (M4)
  behaviour imported once, at start — what / answers. The default is a working
            build; the lab's `broken` commit replaces it with one that answers
            500 while /healthz still answers 200, which is the build a deploy's
            watch has to roll back and a health check cannot catch (M7).
  version   imported once, at start — so the answer identifies the *container*
            rather than the worktree, and a rolling replacement is visible as
            the moment new replicas start answering (M6). Both replicas of a
            service share one mounted worktree, so nothing read per request
            could ever tell the old ones from the new.
  health    imported once, for the same reason: a commit whose health check
            fails must fail the replicas started from it and no others.

It listens on $PORT, which Caramelo sets to the environment's block port.
"""

import base64
import hashlib
import importlib
import os
import socket
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import behaviour
import greeting
import health
import version

DEPS = (("db", 5432), ("cache", 6379))

REPLICA = os.environ.get("CARAMELO_REPLICA") or socket.gethostname()

SLOW_SECONDS = float(os.environ.get("SLOW_SECONDS") or 5)

WS_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


def probe(host, port):
    """Report whether a TCP connection to host:port opens."""
    try:
        with socket.create_connection((host, port), timeout=5):
            return "ok"
    except OSError as err:
        return "error: %s" % (err,)


def deps_report():
    return "".join("%s %s\n" % (name, probe(name, port)) for name, port in DEPS)


def identity():
    """The lines that say which build on which replica answered."""
    return "version %s\nreplica %s\n" % (version.VERSION, REPLICA)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        if self.path.startswith("/ws"):
            self.websocket()
            return
        status = 200
        if self.path.startswith("/healthz"):
            status, body = health.STATUS, health.BODY
        elif self.path.startswith("/deps"):
            body = deps_report()
        elif self.path.startswith("/slow"):
            time.sleep(SLOW_SECONDS)
            body = "slow %s\n%s" % (SLOW_SECONDS, identity())
        else:
            status = behaviour.STATUS
            body = importlib.reload(greeting).greeting() + identity()
            if behaviour.BODY:
                body = behaviour.BODY + identity()
        payload = body.encode()
        self.send_response(status)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def websocket(self):
        """Answer a WebSocket upgrade and echo text frames until it closes."""
        key = self.headers.get("Sec-WebSocket-Key")
        if (self.headers.get("Upgrade") or "").lower() != "websocket" or not key:
            self.send_error(400, "not a websocket upgrade")
            return
        accept = base64.b64encode(
            hashlib.sha1((key + WS_GUID).encode()).digest()
        ).decode()
        self.close_connection = True
        self.wfile.write(
            (
                "HTTP/1.1 101 Switching Protocols\r\n"
                "Upgrade: websocket\r\n"
                "Connection: Upgrade\r\n"
                "Sec-WebSocket-Accept: %s\r\n\r\n" % accept
            ).encode()
        )
        self.wfile.flush()
        print("websocket: open on replica %s" % REPLICA, flush=True)
        try:
            while True:
                frame = ws_read(self.rfile)
                if frame is None:
                    break
                opcode, payload = frame
                if opcode == 0x8:
                    ws_write(self.wfile, 0x8, b"")
                    break
                if opcode == 0x9:
                    ws_write(self.wfile, 0xA, payload)
                    continue
                ws_write(self.wfile, opcode, payload)
        except OSError as err:
            print("websocket: %s" % (err,), flush=True)
        print("websocket: closed on replica %s" % REPLICA, flush=True)

    def log_message(self, fmt, *args):
        print("request %s" % (fmt % args), flush=True)


def ws_read(rfile):
    """Read one WebSocket frame; None at end of stream."""
    head = rfile.read(2)
    if len(head) < 2:
        return None
    opcode = head[0] & 0x0F
    masked = head[1] & 0x80
    length = head[1] & 0x7F
    if length == 126:
        ext = rfile.read(2)
        if len(ext) < 2:
            return None
        length = int.from_bytes(ext, "big")
    elif length == 127:
        ext = rfile.read(8)
        if len(ext) < 8:
            return None
        length = int.from_bytes(ext, "big")
    mask = rfile.read(4) if masked else b""
    payload = rfile.read(length) if length else b""
    if len(payload) < length:
        return None
    if masked:
        payload = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
    return opcode, payload


def ws_write(wfile, opcode, payload):
    """Write one final, unmasked frame, as a server must."""
    header = bytes([0x80 | opcode])
    if len(payload) <= 125:
        header += bytes([len(payload)])
    else:
        header += bytes([126]) + len(payload).to_bytes(2, "big")
    wfile.write(header + payload)
    wfile.flush()


def main():
    port = int(os.environ.get("PORT", "8000"))
    print(
        "sampleapp: listening on 0.0.0.0:%d (version %s, replica %s)"
        % (port, version.VERSION, REPLICA),
        flush=True,
    )
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
