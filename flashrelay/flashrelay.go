//go:build linux && amd64

// Package flashrelay is a pure-Go (CGO_ENABLED=0) io_uring TCP relay engine for
// Linux. It accepts client connections, runs a caller-supplied decision hook on
// the initial request bytes, then splices client↔upstream bidirectionally — with
// NO Go netpoller on any data-plane fd (listener, client, or upstream).
//
// Each worker is a shared-nothing per-core engine (its own SO_REUSEPORT listener,
// io_uring ring, hook-goroutine pool, and connection map), so the engine scales
// across cores without a shared scheduler. Import it, give it a Config + Hook,
// and call Run.
//
//	srv, _ := flashrelay.New(flashrelay.Config{Addr: "203.0.113.7", Port: 443, Workers: 40}, hook)
//	go srv.Run()
//	...
//	srv.Stop()
//
// The Hook is where the caller does auth/blacklist/IP-alloc and DIALS the
// upstream (with a blocking raw syscall, so the upstream fd never touches the
// netpoller), returning the connected fd for the engine to adopt and relay.
package flashrelay

import (
	"fmt"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thealonlevi/flash-relay/internal/rawsock"
)

// Decision is what a Hook returns for a connection.
type Decision struct {
	// UpstreamFD is a connected upstream socket fd (the caller dialed it, e.g.
	// with a blocking syscall.Connect) for the engine to adopt and relay to.
	// Ignored when Reject is true.
	UpstreamFD int
	// Reply, if non-empty, is sent to the client before relaying (or, when
	// Reject is set, sent as the final bytes before closing).
	Reply []byte
	// Reject closes the connection after sending Reply, without relaying.
	Reject bool
}

// Hook is the caller's per-connection decision callback. It receives the initial
// request bytes (up to Config.InitialReqLen) and the client's peer address. It
// MAY block (auth, blacklist lookup, IP allocation, the upstream dial) — it runs
// on an off-ring goroutine pool, so a slow hook parks one connection, never the
// ring. Return a relay Decision (adopt UpstreamFD) or a reject.
type Hook func(req []byte, peer netip.AddrPort) Decision

// Config configures a Server. Zero-value fields take the documented defaults.
type Config struct {
	Addr          string        // bind IP (specific public IP; "" => 0.0.0.0). IPv4 or IPv6.
	Port          int           // listen port
	Workers       int           // shared-nothing per-core rings (SO_REUSEPORT). 0 => runtime.NumCPU().
	Pin           bool          // if true, pin worker i to CPU StartCore+i (one ring/core).
	StartCore     int           // first core to pin to when Pin is set.
	InitialReqLen int           // bytes to read before invoking the Hook. 0 => 64.
	BufSize       int           // per-direction relay buffer bytes. 0 => 16384.
	MaxConns      int           // per-worker live-connection cap (backpressure; shed above). 0 => 50000.
	AcceptBatch   int           // accepts kept in flight per worker. 0 => 64.
	IdleTimeout   time.Duration // close connections idle longer than this. 0 => disabled.
	HookWorkers   int           // off-ring hook goroutines per worker. 0 => 256.
	RingSize      uint          // io_uring SQ entries per worker. 0 => 4096.
}

func (c *Config) defaults() {
	if c.Workers <= 0 {
		c.Workers = runtime.NumCPU()
	}
	if c.InitialReqLen <= 0 {
		c.InitialReqLen = 64
	}
	if c.BufSize <= 0 {
		c.BufSize = 16384
	}
	if c.MaxConns <= 0 {
		c.MaxConns = 50000
	}
	if c.AcceptBatch <= 0 {
		c.AcceptBatch = 64
	}
	if c.HookWorkers <= 0 {
		c.HookWorkers = 256
	}
	if c.RingSize == 0 {
		c.RingSize = 4096
	}
}

// Stats is a point-in-time snapshot of engine counters (summed across workers).
type Stats struct {
	Accepted   uint64
	Completed  uint64 // connections fully relayed and closed
	Rejected   uint64 // hook returned Reject
	Shed       uint64 // accepted-then-closed at the backpressure cap
	IdleClosed uint64 // closed by the idle timeout
	Errors     uint64
	BytesC2U   uint64 // client -> upstream bytes relayed
	BytesU2C   uint64 // upstream -> client bytes relayed
	LiveConns  uint64 // currently open connections
}

type counters struct {
	accepted, completed, rejected, shed, idleClosed, errors atomic.Uint64
	bytesC2U, bytesU2C, live                                atomic.Uint64
}

// Server is a running relay engine: Config.Workers shared-nothing per-core rings.
type Server struct {
	cfg  Config
	hook Hook
	cnt  counters
	stop atomic.Bool
	wg   sync.WaitGroup
}

// New validates cfg and returns a Server. Call Run to start it.
func New(cfg Config, hook Hook) (*Server, error) {
	if hook == nil {
		return nil, fmt.Errorf("flashrelay: nil hook")
	}
	if cfg.Port <= 0 {
		return nil, fmt.Errorf("flashrelay: invalid port %d", cfg.Port)
	}
	cfg.defaults()
	return &Server{cfg: cfg, hook: hook}, nil
}

// Run creates the SO_REUSEPORT listeners and starts the workers. It blocks until
// Stop is called and all workers have drained. Returns the first fatal error.
func (s *Server) Run() error {
	// Listeners are created sequentially (concurrent SO_REUSEPORT binds race the
	// kernel's reuseport-group setup), then handed one per worker.
	lns := make([]*rawsock.Listener, 0, s.cfg.Workers)
	for i := 0; i < s.cfg.Workers; i++ {
		ln, err := rawsock.Listen(s.cfg.Addr, s.cfg.Port, 4096)
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return fmt.Errorf("flashrelay: listener %d: %w", i, err)
		}
		lns = append(lns, ln)
	}
	for i := 0; i < s.cfg.Workers; i++ {
		core := -1
		if s.cfg.Pin {
			core = s.cfg.StartCore + i
		}
		s.wg.Add(1)
		go s.runWorker(i, core, lns[i])
	}
	s.wg.Wait()
	for _, l := range lns {
		l.Close()
	}
	return nil
}

// Stop signals all workers to stop accepting and drain in-flight connections,
// then exit. Run returns once drained. Async-signal-safe.
func (s *Server) Stop() { s.stop.Store(true) }

// Dial performs a BLOCKING, raw-syscall TCP connect to host:port (IPv4 or IPv6)
// and returns the connected fd for a Hook to return as Decision.UpstreamFD.
// Unlike net.Dial it never registers the fd with the Go netpoller — which is the
// whole point: the relayed upstream fd must stay off the poller. The blocking
// connect parks the calling (off-ring hook) goroutine's thread via the Go
// scheduler; it does not touch the ring. Callers that need a non-blocking or
// custom dial may produce the fd themselves (any connected SOCK_STREAM fd works).
func Dial(host string, port int) (int, error) { return rawsock.Dial(host, port) }

// Fingerprint profile ids (modern OSes). Each maps to an eBPF mark (selects the
// per-packet TTL+IP-ID profile and the SYN option-layout) + an SO_RCVBUF that makes
// the kernel emit the profile's window scale. The eBPF rewrites TTL+IP-ID on EVERY
// packet (coherent across the whole flow) and the option layout on the SYN. macOS
// (mark 2) and iOS (mark 4) share the option layout but differ on IP ID (macOS=0,
// iOS=random), hence distinct marks. See fingerprint/RESEARCH.md.
const (
	FPWindows = 1 // TTL128, IPID incrementing, mss,nop,ws,nop,nop,sok (no TS); wscale 8
	FPMacOS   = 2 // TTL64,  IPID 0,            mss,nop,ws,nop,nop,ts,sok,eol;   wscale 6
	FPAndroid = 3 // TTL64,  IPID random,       mss,sok,ts,nop,ws (== Linux);    wscale 9
	FPiOS     = 4 // TTL64,  IPID random,       == macOS layout;                 wscale 6; +ECN, +tos 0x50
)

// fpProfile is the (eBPF mark, SO_RCVBUF, IP TOS byte) tuple for a profile. mark
// selects the eBPF per-packet TTL/IP-ID + SYN option profile; tos is the per-socket
// DiffServ+ECN byte (real header field, not forged).
type fpProfile struct{ mark, rcvbuf, tos int }

// rcvbuf->wscale on a host with net.core.rmem_max raised: 2M->6, 4M->7, 8M->8, 16M->9.
// iOS adds tos 0x50 (real capture); its ECN SYN bits come from net.ipv4.tcp_ecn=1 (deploy).
var fpProfiles = map[int]fpProfile{
	FPWindows: {mark: 1, rcvbuf: 8 << 20},            // TTL128, IPID keep, wscale 8
	FPMacOS:   {mark: 2, rcvbuf: 2 << 20},            // TTL64, IPID 0, wscale 6
	FPAndroid: {mark: 3, rcvbuf: 16 << 20},           // TTL64, IPID random, layout==Linux; wscale 9 (needs rmem_max>=16M)
	FPiOS:     {mark: 4, rcvbuf: 2 << 20, tos: 0x50}, // TTL64, IPID random, macOS layout, wscale 6 + DSCP 0x50; ECN via tcp_ecn=1
}

// DialFingerprint dials upstream and shapes the SYN to a chosen OS TCP/IP
// fingerprint: SO_MARK selects the eBPF option-layout/TTL rewrite (fingerprint/),
// SO_RCVBUF makes the kernel emit the profile's window scale, and IP_TOS sets the
// DiffServ/ECN byte (e.g. iOS 0x50). A Hook calls this to give each upstream the
// fingerprint matching the client it serves. profile 0 == plain Dial. Requires the
// eBPF attached + CAP_NET_ADMIN; full wscale/window fidelity needs net.core.rmem_max
// raised and a real NIC (loopback MSS distorts the window). The Apple ECN SYN bits
// (iOS [SEW]) are real ECN negotiation — enable with sysctl net.ipv4.tcp_ecn=1.
func DialFingerprint(host string, port, profile int) (int, error) {
	p := fpProfiles[profile]
	return rawsock.DialFP(host, port, p.mark, p.rcvbuf, p.tos)
}

// Stat returns a snapshot of the engine's counters.
func (s *Server) Stat() Stats {
	return Stats{
		Accepted:   s.cnt.accepted.Load(),
		Completed:  s.cnt.completed.Load(),
		Rejected:   s.cnt.rejected.Load(),
		Shed:       s.cnt.shed.Load(),
		IdleClosed: s.cnt.idleClosed.Load(),
		Errors:     s.cnt.errors.Load(),
		BytesC2U:   s.cnt.bytesC2U.Load(),
		BytesU2C:   s.cnt.bytesU2C.Load(),
		LiveConns:  s.cnt.live.Load(),
	}
}
