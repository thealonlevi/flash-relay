#!/usr/bin/env python3
# pick-ports.py — print N verified-bindable TCP ports, one space-separated line.
#
# Why this exists: a relay that wedges into D-state (uninterruptible io_uring_enter)
# can never close its sockets, so they sit in CLOSE-WAIT forever and hold their
# local ports until reboot. `ss -ltn` does NOT show these (not LISTEN state), so
# trusting it — or trusting a fixed port list — makes the referee try to bind a
# poisoned port and fail with EADDRINUSE (looks like a relay crash). We instead
# *prove* each candidate is bindable by actually binding it.
#
# Usage: pick-ports.py START COUNT [STRIDE]
#   START : first candidate base port
#   COUNT : how many distinct bindable ports to return
#   STRIDE: spacing between returned ports (default 100, room for per-role ranges)
# Each returned port is one that bind()+SO_REUSEADDR+SO_REUSEPORT succeeded on at
# call time. Reserve them (ip_local_reserved_ports) immediately so the ephemeral
# allocator can't grab them between here and the relay's bind.
import socket
import sys


def bindable(port: int) -> bool:
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        # SO_REUSEPORT (15) mirrors how rawsock.Listen binds the real relay.
        try:
            s.setsockopt(socket.SOL_SOCKET, 15, 1)
        except OSError:
            pass
        s.bind(("127.0.0.1", port))
        return True
    except OSError:
        return False
    finally:
        s.close()


def main() -> int:
    start = int(sys.argv[1])
    count = int(sys.argv[2])
    stride = int(sys.argv[3]) if len(sys.argv) > 3 else 100
    found = []
    p = start
    while len(found) < count and p <= 65000:
        if bindable(p):
            found.append(p)
            p += stride
        else:
            p += 1  # poisoned — step past it
    if len(found) < count:
        sys.stderr.write(f"pick-ports: only {len(found)}/{count} bindable from {start}\n")
        return 1
    print(" ".join(str(x) for x in found))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
