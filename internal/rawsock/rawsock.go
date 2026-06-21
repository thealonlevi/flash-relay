//go:build linux

// Package rawsock creates and manages TCP sockets via raw syscalls only —
// never the net package. This is the non-negotiable of flash-relay: a data-plane
// fd that goes through net.Conn/os.File gets registered with the Go netpoller
// (epoll), which is exactly what B1 proves we eliminate. Raw syscall.Socket fds
// are never registered with the runtime poller. See gate/DESIGN.md and CLAUDE.md.
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
func Dial(ip string, port int) (int, error) {
	sa, family, err := resolve(ip, port)
	if err != nil {
		return -1, err
	}
	fd, err := syscall.Socket(family, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		return -1, fmt.Errorf("socket: %w", err)
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
