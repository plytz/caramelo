#!/usr/bin/env python3
"""A minimal PostgreSQL client, standard library only.

It exists because the sample app runs in `python:3.12-alpine` — the toolchain
image the lab has in its cache — and there is no psycopg in it, no wheel to
install and no network to install one from. `deploy.before` has to write a row
in a real database and `deploy.check` has to read it back, so the wire protocol
is spoken here directly.

What it implements is the smallest useful subset: the startup handshake,
SCRAM-SHA-256 (what PostgreSQL 16 asks for by default), cleartext and MD5 as
fallbacks, the simple query protocol, and text-format results. No prepared
statements, no types, no transactions, no pooling — every value comes back as a
string, which is all the two scripts that use it need.

This is test data, not product code: Caramelo never speaks to a database itself.
"""

import base64
import hashlib
import hmac
import os
import socket
import struct
from urllib.parse import unquote, urlparse

PROTOCOL_VERSION = 196608


class PostgresError(Exception):
    """An error the server reported, or a handshake we could not complete."""


def connect(url=None, timeout=15):
    """Open a connection from a postgres:// URL (DATABASE_URL by default)."""
    url = url or os.environ.get("DATABASE_URL")
    if not url:
        raise PostgresError("no DATABASE_URL in the environment")
    parts = urlparse(url)
    if parts.scheme not in ("postgres", "postgresql"):
        raise PostgresError("not a postgres URL: %s" % parts.scheme)
    conn = Connection(
        host=parts.hostname or "localhost",
        port=parts.port or 5432,
        user=unquote(parts.username or "postgres"),
        password=unquote(parts.password or ""),
        database=(parts.path or "/postgres").lstrip("/") or "postgres",
        timeout=timeout,
    )
    conn.start()
    return conn


class Connection:
    def __init__(self, host, port, user, password, database, timeout=15):
        self.host, self.port = host, port
        self.user, self.password, self.database = user, password, database
        self.timeout = timeout
        self.sock = None
        self._buf = b""


    def start(self):
        self.sock = socket.create_connection((self.host, self.port), self.timeout)
        self.sock.settimeout(self.timeout)
        payload = b"".join(
            [
                struct.pack("!i", PROTOCOL_VERSION),
                b"user\x00" + self.user.encode() + b"\x00",
                b"database\x00" + self.database.encode() + b"\x00",
                b"\x00",
            ]
        )
        self._send_raw(struct.pack("!i", len(payload) + 4) + payload)
        self._authenticate()
        while True:
            tag, body = self._read_message()
            if tag == b"Z":
                return
            if tag == b"E":
                raise PostgresError(error_text(body))

    def close(self):
        if self.sock is not None:
            try:
                self._send(b"X", b"")
            except OSError:
                pass
            self.sock.close()
            self.sock = None

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()


    def query(self, sql):
        """Run one statement and return its rows as lists of strings."""
        self._send(b"Q", sql.encode() + b"\x00")
        rows, error = [], None
        while True:
            tag, body = self._read_message()
            if tag == b"D":
                rows.append(parse_data_row(body))
            elif tag == b"E":
                error = error_text(body)
            elif tag == b"Z":
                break
        if error:
            raise PostgresError(error)
        return rows

    execute = query

    def scalar(self, sql, default=None):
        """The first column of the first row, or default."""
        rows = self.query(sql)
        if not rows or not rows[0]:
            return default
        return rows[0][0]


    def _authenticate(self):
        while True:
            tag, body = self._read_message()
            if tag == b"E":
                raise PostgresError(error_text(body))
            if tag != b"R":
                raise PostgresError("unexpected message %r during authentication" % tag)
            (code,) = struct.unpack("!i", body[:4])
            rest = body[4:]
            if code == 0:
                return
            if code == 3:
                self._send(b"p", self.password.encode() + b"\x00")
            elif code == 5:
                salt = rest[:4]
                inner = hashlib.md5(
                    (self.password + self.user).encode()
                ).hexdigest().encode()
                digest = b"md5" + hashlib.md5(inner + salt).hexdigest().encode()
                self._send(b"p", digest + b"\x00")
            elif code == 10:
                mechanisms = [m for m in rest.split(b"\x00") if m]
                if b"SCRAM-SHA-256" not in mechanisms:
                    raise PostgresError("no SCRAM-SHA-256 in %r" % mechanisms)
                self._scram()
                return
            else:
                raise PostgresError("unsupported authentication request %d" % code)

    def _scram(self):
        """RFC 5802 SCRAM-SHA-256, the channel-binding-free variant."""
        nonce = base64.b64encode(os.urandom(18)).decode()
        first_bare = "n=,r=" + nonce
        initial = "n,," + first_bare
        self._send(
            b"p",
            b"SCRAM-SHA-256\x00"
            + struct.pack("!i", len(initial))
            + initial.encode(),
        )

        tag, body = self._read_message()
        if tag == b"E":
            raise PostgresError(error_text(body))
        (code,) = struct.unpack("!i", body[:4])
        if tag != b"R" or code != 11:
            raise PostgresError("expected SASLContinue, got %r/%d" % (tag, code))
        server_first = body[4:].decode()
        attrs = scram_attrs(server_first)
        server_nonce, salt, iterations = attrs["r"], attrs["s"], int(attrs["i"])
        if not server_nonce.startswith(nonce):
            raise PostgresError("the server's nonce does not extend ours")

        salted = hashlib.pbkdf2_hmac(
            "sha256", self.password.encode(), base64.b64decode(salt), iterations
        )
        client_key = hmac_sha256(salted, b"Client Key")
        stored_key = hashlib.sha256(client_key).digest()
        final_bare = "c=biws,r=" + server_nonce
        auth_message = ",".join([first_bare, server_first, final_bare])
        signature = hmac_sha256(stored_key, auth_message.encode())
        proof = bytes(a ^ b for a, b in zip(client_key, signature))
        final = final_bare + ",p=" + base64.b64encode(proof).decode()
        self._send(b"p", final.encode())

        server_key = hmac_sha256(salted, b"Server Key")
        expected = base64.b64encode(
            hmac_sha256(server_key, auth_message.encode())
        ).decode()
        while True:
            tag, body = self._read_message()
            if tag == b"E":
                raise PostgresError(error_text(body))
            if tag != b"R":
                raise PostgresError("unexpected message %r finishing SCRAM" % tag)
            (code,) = struct.unpack("!i", body[:4])
            if code == 12:
                got = scram_attrs(body[4:].decode()).get("v")
                if got != expected:
                    raise PostgresError("the server's SCRAM signature does not verify")
                continue
            if code == 0:
                return
            raise PostgresError("unexpected authentication code %d finishing SCRAM" % code)


    def _send(self, tag, payload):
        self._send_raw(tag + struct.pack("!i", len(payload) + 4) + payload)

    def _send_raw(self, data):
        self.sock.sendall(data)

    def _read_message(self):
        head = self._read_exactly(5)
        tag, length = head[:1], struct.unpack("!i", head[1:])[0]
        body = self._read_exactly(length - 4) if length > 4 else b""
        return tag, body

    def _read_exactly(self, n):
        out = b""
        while len(out) < n:
            chunk = self.sock.recv(n - len(out))
            if not chunk:
                raise PostgresError("the connection closed mid-message")
            out += chunk
        return out


def hmac_sha256(key, msg):
    return hmac.new(key, msg, hashlib.sha256).digest()


def scram_attrs(s):
    """SCRAM's comma-separated key=value list."""
    out = {}
    for field in s.split(","):
        if "=" in field:
            key, value = field.split("=", 1)
            out[key] = value
    return out


def parse_data_row(body):
    """One DataRow as a list of strings; a NULL column is None."""
    (count,) = struct.unpack("!h", body[:2])
    out, at = [], 2
    for _ in range(count):
        (length,) = struct.unpack("!i", body[at : at + 4])
        at += 4
        if length < 0:
            out.append(None)
            continue
        out.append(body[at : at + length].decode("utf-8", "replace"))
        at += length
    return out


def error_text(body):
    """An ErrorResponse as one readable line."""
    fields = {}
    for part in body.split(b"\x00"):
        if part:
            fields[part[:1].decode()] = part[1:].decode("utf-8", "replace")
    return "%s: %s" % (fields.get("S", "ERROR"), fields.get("M", "unknown error"))
