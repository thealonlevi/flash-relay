//go:build linux

package storm

import (
	"fmt"
	"net"
	"syscall"
)

// Netpoller-free junk connections.
//
// WHY. A junk connection is the connect-flood primitive: connect, then close
// without sending a byte. Through net.Dial that costs a netpoller registration
// (epoll_ctl) on a socket that never waits for readiness — and the epoll mutex is
// shared. Profiling the load generator at 8 relay cores showed 13.3% osq_lock plus
// 3.4% mutex_spin_on_owner: ~17% of the client's CPU spinning on a kernel mutex.
// That is the SAME collapse relay-netpoll exhibits (32.6% osq_lock at 16 cores) —
// the load generator had the netpoller ceiling it exists to measure, so adding
// cores to it stopped buying load. With junk at 93% of the workload, that ceiling
// was the sweep's binding constraint and hid flash-relay's own knee.
//
// A junk connection needs exactly socket(2) + connect(2) + close(2). Blocking
// connect parks the OS thread via the Go scheduler and never touches epoll.
//
// SEMANTICS ARE DELIBERATELY UNCHANGED. close(2) on a socket with no unread data
// sends FIN, exactly as net.Conn.Close does. It is tempting to set SO_LINGER 0 for
// an RST — cheaper, and it would dodge client TIME_WAIT entirely — but that would
// change what the SUT sees (reset vs clean EOF) and so change the workload being
// measured. The point here is to make the CLIENT cheaper, not the test easier.
//
// The real (non-junk) connections keep using net: they need buffered read/write
// with deadlines, and at 7% of the mix they are not the client's bottleneck.

// sockAddr converts an IP and port into the syscall sockaddr for its family,
// alongside the socket domain to open.
func sockAddr(ip net.IP, port int) (syscall.Sockaddr, int, error) {
	if ip4 := ip.To4(); ip4 != nil {
		sa := &syscall.SockaddrInet4{Port: port}
		copy(sa.Addr[:], ip4)
		return sa, syscall.AF_INET, nil
	}
	if ip16 := ip.To16(); ip16 != nil {
		sa := &syscall.SockaddrInet6{Port: port}
		copy(sa.Addr[:], ip16)
		return sa, syscall.AF_INET6, nil
	}
	return nil, 0, fmt.Errorf("storm: unusable IP %v", ip)
}

// resolveTarget turns an "ip:port" target into a connect-ready sockaddr.
func resolveTarget(target string) (syscall.Sockaddr, int, error) {
	ta, err := net.ResolveTCPAddr("tcp", target)
	if err != nil {
		return nil, 0, err
	}
	if ta.IP == nil {
		return nil, 0, fmt.Errorf("storm: target %q has no IP", target)
	}
	return sockAddr(ta.IP, ta.Port)
}

// resolveSource turns a source IP into a bind-ready sockaddr (port 0 = let the
// kernel pick within that IP's ephemeral space).
func resolveSource(ip string) (syscall.Sockaddr, int, error) {
	pip := net.ParseIP(ip)
	if pip == nil {
		return nil, 0, fmt.Errorf("storm: bad source IP %q", ip)
	}
	return sockAddr(pip, 0)
}

// junkDial is one connect-flood connection: socket, optional source bind, blocking
// connect, close. No netpoller, no allocation beyond the fd.
func junkDial(domain int, dst, src syscall.Sockaddr) error {
	fd, err := syscall.Socket(domain, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, syscall.IPPROTO_TCP)
	if err != nil {
		return err
	}
	// close(2) unconditionally: on the error paths below the fd must not leak, and
	// on success closing IS the second half of the connect-flood primitive.
	defer syscall.Close(fd)
	if src != nil {
		// Without SO_REUSEADDR a bound source IP exhausts its ephemeral ports far
		// sooner under churn, because TIME_WAIT entries block rebinding.
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		if err := syscall.Bind(fd, src); err != nil {
			return err
		}
	}
	return syscall.Connect(fd, dst)
}
