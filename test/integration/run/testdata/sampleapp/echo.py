#!/usr/bin/env python3
"""A UDP echo service: the sample app's second service.

It exists so the suite can prove that a workload which is not HTTP works too:
a UDP port published on the box's loopback, no health check (there is nothing
to connect to), its own container on the same environment network.

It listens on $ECHO_PORT, which caramelo.yaml sets from
${services.echo.port} — the network view of its own port.
"""

import os
import socket

MAX_DATAGRAM = 4096


def main():
    port = int(os.environ.get("ECHO_PORT") or 9001)
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.bind(("0.0.0.0", port))
    print("sampleapp: echoing udp on 0.0.0.0:%d" % port, flush=True)
    while True:
        data, peer = sock.recvfrom(MAX_DATAGRAM)
        print("echo: %d bytes from %s" % (len(data), peer[0]), flush=True)
        sock.sendto(data, peer)


if __name__ == "__main__":
    main()
