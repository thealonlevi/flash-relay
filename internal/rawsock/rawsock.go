//go:build linux

// Package rawsock creates and manages TCP sockets via raw syscalls only —
// never the net package. This is the non-negotiable of flash-relay: a data-plane
// fd that goes through net.Conn/os.File gets registered with the Go netpoller
// (epoll), which is exactly what B1 proves we eliminate. Raw syscall.Socket fds
// are never registered with the runtime poller. See research/gate/DESIGN.md.
package rawsock

import (
	"fmt"
	"net/netip"
	"syscall"
)

// soReusePort is SO_REUSEPORT (not exported by the stdlib syscall package on
// all arches). Linux value.
const soReusePort = 15

// Listener is a raw, blocking TCP listening socket with SO_REUSEPORT set.
type Listener struct {
	FD   int
	Port int
}

// Listen creates a TCP listener bound to ip:port (IPv4 or IPv6) with SO_REUSEPORT.
// A port of 0 picks an ephemeral port (read back into Listener.Port). ip "" means
// 127.0.0.1 (loopback, the gate default); pass a specific IP to bind one address.
func Listen(ip string, port int, backlog int) (*Listener, error) {
	sa, family, err := resolve(ip, port)
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Socket(family, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("SO_REUSEADDR: %w", err)
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, soReusePort, 1); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("SO_REUSEPORT: %w", err)
	}
	if err := syscall.Bind(fd, sa); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("bind: %w", err)
	}
	if err := syscall.Listen(fd, backlog); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("listen: %w", err)
	}
	l := &Listener{FD: fd, Port: port}
	if port == 0 {
		lsa, err := syscall.Getsockname(fd)
		if err != nil {
			syscall.Close(fd)
			return nil, fmt.Errorf("getsockname: %w", err)
		}
		switch a := lsa.(type) {
		case *syscall.SockaddrInet4:
			l.Port = a.Port
		case *syscall.SockaddrInet6:
			l.Port = a.Port
		}
	}
	return l, nil
}

// Close closes the listening socket.
func (l *Listener) Close() error { return syscall.Close(l.FD) }

// Dial performs a BLOCKING connect to ip:port via a raw syscall and returns the
// connected fd. The blocking connect parks the calling goroutine's OS thread via
// the Go scheduler — it does NOT touch the netpoller. This is how riptide dials
// upstream (and how the gate's decision hook dials the sink). See DESIGN.md §3.2.
func Dial(ip string, port int) (int, error)           { return DialFP(ip, port, 0, 0, 0) }
func DialMark(ip string, port, mark int) (int, error) { return DialFP(ip, port, mark, 0, 0) }

// soMark is SO_SOCKET-level SO_MARK (not exported by syscall on all arches).
const soMark = 36

// DialFP is Dial plus the three knobs the TCP-fingerprint feature needs, set before
// connect so they shape the SYN (see fingerprint/):
//   - mark>0   : SO_MARK — rides the connection as skb->mark; the tc-egress eBPF
//     reads it to pick the OS option-layout + TTL profile.
//   - rcvbuf>0 : SO_RCVBUF — the kernel derives the SYN's window scale (and initial
//     window) from it, so this sets the *functional* wscale/window that
//     can't be forged in-packet. Hitting high wscales needs
//     net.core.rmem_max raised (e.g. 16 MiB). 0 = leave default.
//   - tos>0    : IP_TOS / IPV6_TCLASS — the IP DiffServ+ECN byte (e.g. iOS sends
//     0x50). Real per-socket header field, not forged. 0 = leave default.
//
// mark needs CAP_NET_ADMIN. 0/0/0 == plain Dial.
func DialFP(ip string, port, mark, rcvbuf, tos int) (int, error) {
	sa, family, err := resolve(ip, port)
	if err != nil {
		return -1, err
	}
	fd, err := syscall.Socket(family, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		return -1, fmt.Errorf("socket: %w", err)
	}
	if mark > 0 {
		if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, soMark, mark); err != nil {
			syscall.Close(fd)
			return -1, fmt.Errorf("SO_MARK: %w", err)
		}
	}
	if rcvbuf > 0 {
		// best-effort: clamped to net.core.rmem_max; a too-low rmem_max caps wscale.
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvbuf)
	}
	if tos > 0 {
		// best-effort DSCP/ECN byte: IP_TOS for v4, IPV6_TCLASS for v6.
		if family == syscall.AF_INET6 {
			_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_TCLASS, tos)
		} else {
			_ = syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TOS, tos)
		}
	}
	if err := syscall.Connect(fd, sa); err != nil {
		syscall.Close(fd)
		return -1, fmt.Errorf("connect %s:%d: %w", ipOrLoopback(ip), port, err)
	}
	return fd, nil
}

// SetNoDelay sets TCP_NODELAY on fd.
func SetNoDelay(fd int) error {
	return syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
}

func ipOrLoopback(ip string) string {
	if ip == "" {
		return "127.0.0.1"
	}
	return ip
}

// resolve parses ip into a Sockaddr and its socket family (AF_INET / AF_INET6).
func resolve(ip string, port int) (syscall.Sockaddr, int, error) {
	addr, err := netip.ParseAddr(ipOrLoopback(ip))
	if err != nil {
		return nil, 0, fmt.Errorf("parse ip %q: %w", ip, err)
	}
	if addr.Is4() {
		return &syscall.SockaddrInet4{Port: port, Addr: addr.As4()}, syscall.AF_INET, nil
	}
	return &syscall.SockaddrInet6{Port: port, Addr: addr.As16()}, syscall.AF_INET6, nil
}
