#!/usr/bin/env python3
"""Bounded AF_UNIX probe used by guarded activation."""

import argparse
import re
import socket
import sys


STATUS_LINE = re.compile(rb"HTTP/1\.[01] ([0-9]{3})(?: [^\r\n]*)?\r\n")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--socket", required=True)
    parser.add_argument("--host", required=True)
    parser.add_argument("--login", required=True)
    parser.add_argument("--expect-denied", action="store_true")
    args = parser.parse_args()

    client = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    client.settimeout(3)
    try:
        client.connect(args.socket)
    except OSError as error:
        print(f"connect failed: {error}", file=sys.stderr)
        return 1

    request = (
        "GET /api/inventory HTTP/1.1\r\n"
        "Host: localhost\r\n"
        f"X-Forwarded-Host: {args.host}\r\n"
        "X-Forwarded-Proto: https\r\n"
        f"Tailscale-User-Login: {args.login}\r\n"
        "Tailscale-User-Name: Persea Operator\r\n"
        "Tailscale-User-Profile-Pic: https://invalid.example.test/profile\r\n"
        "Tailscale-Headers-Info: https://tailscale.com/s/serve-headers\r\n"
        "Connection: close\r\n\r\n"
    ).encode("ascii")
    chunks: list[bytes] = []
    size = 0
    peer_closed = False
    try:
        try:
            client.sendall(request)
        except (BrokenPipeError, ConnectionResetError):
            peer_closed = True
        while not peer_closed and size < 65536:
            try:
                chunk = client.recv(min(4096, 65536 - size))
            except ConnectionResetError:
                peer_closed = True
                break
            if not chunk:
                peer_closed = True
                break
            chunks.append(chunk)
            size += len(chunk)
            if args.expect_denied or b"\r\n\r\n" in b"".join(chunks):
                break
        response = b"".join(chunks)
    except (socket.timeout, OSError) as error:
        print(f"Unix exchange failed: {error}", file=sys.stderr)
        return 1
    finally:
        client.close()

    if args.expect_denied:
        if response:
            print("denied peer received response bytes", file=sys.stderr)
            return 1  # Any byte disproves listener-level peer rejection.
        if not peer_closed:
            print("denied peer did not reach EOF or reset", file=sys.stderr)
            return 1
        return 0  # Exact zero-byte EOF/reset after connect proves denial.

    if b"\r\n\r\n" not in response:
        print("malformed or incomplete HTTP response", file=sys.stderr)
        return 1
    match = STATUS_LINE.match(response)
    if match is None:
        print("malformed HTTP status line", file=sys.stderr)
        return 1
    status = int(match.group(1))
    return 0 if status == 200 else 1


if __name__ == "__main__":
    sys.exit(main())
