//go:build linux

// Package hook models riptide's real decision path between accept and relay:
// a calibrated auth CPU spin + an async, ms-scale dial latency park + a real
// blocking raw connect() to the upstream. It is deliberately NOT a no-op — see
// gate/DESIGN.md §3 (the single most common way these benchmarks lie).
//
// The hook is meant to run OFF the io_uring ring, on a worker-goroutine pool, so
// a slow dial parks one connection while the ring keeps accepting/relaying others.
package hook

import (
	"math"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/thealonlevi/flash-relay/internal/rawsock"
)

// DelayFunc returns a (possibly random) dial latency to model. The realistic-dial
// variant samples a ms-scale distribution; the headline variant returns 0.
type DelayFunc func() time.Duration

// NoDelay models an instant dial (headline / CPU-isolation run): DIAL_DELAY = 0.
func NoDelay() DelayFunc { return func() time.Duration { return 0 } }

// Lognormal models real egress RTT (realistic-dial run). p50 is the median in ms;
// sigma is the log-space spread. Defaults p50≈20ms, sigma≈0.9 give roughly
// p90≈80ms, p99≈200ms+ (DESIGN §3.2). capMs clamps the tail to the dial timeout.
func Lognormal(p50ms, sigma, capMs float64, seed int64) DelayFunc {
	rng := rand.New(rand.NewSource(seed))
	mu := math.Log(p50ms)
	return func() time.Duration {
		ms := math.Exp(mu + sigma*rng.NormFloat64())
		if ms > capMs {
			ms = capMs
		}
		return time.Duration(ms * float64(time.Millisecond))
	}
}

// Config is the decision-hook cost model.
type Config struct {
	AuthCPU  time.Duration // calibrated CPU busy-spin (HOOK_CPU_US), e.g. 5µs
	Delay    DelayFunc     // ms-scale async dial park (NoDelay for headline)
	SinkIP   string        // upstream sink address
	SinkPort int
	// SinkPorts spreads the upstream dial across SinkPort..SinkPort+SinkPorts-1.
	// 0 or 1 = single port.
	//
	// WHY THIS EXISTS. The relay dials upstream once per connection, and with one
	// destination port every dial shares ONE (srcIP,dstIP,dstPort) 4-tuple, which
	// caps near 64k ephemeral ports. Measured 2-box: at 24,407 dials/s the box
	// accumulated 285,035 TIME_WAIT sockets and throughput collapsed from 33,825 to
	// 1,086 conn/s — the port allocator, not the relay. The client side already
	// spreads across a port span; this is the symmetric half for the upstream leg.
	SinkPorts int
	// SinkIPs, if non-empty, spreads the upstream dial across several upstream
	// HOSTS as well as ports. Empty = use SinkIP alone.
	//
	// Two independent reasons, both measured. (1) Tuple space: destination IP is
	// part of the 4-tuple, so N hosts multiply the ephemeral headroom the same way
	// N ports do. (2) Path independence: this network degrades under connection
	// concurrency per PATH, not in aggregate — two load boxes storming box 1
	// simultaneously delivered 144,554 conn/s against 46,814 and 105,591 measured
	// alone, i.e. 95% of the sum. The same should hold for the upstream leg, whose
	// single-path ceiling (~50-60k with a 1-2s tail past ~1k concurrent) is
	// otherwise what caps a relayed run.
	SinkIPs []string
	Mark    int // SO_MARK on the upstream dial (fingerprint profile; 0 = none)
	// dialSeq round-robins the destination port. Per-Config, and the hook pool
	// shares one Config, so it must be atomic.
	dialSeq *atomic.Uint64
}

// Init prepares per-Config mutable state. Call once before sharing a Config
// across hook goroutines.
func (c *Config) Init() {
	if c.dialSeq == nil {
		c.dialSeq = new(atomic.Uint64)
	}
}

// sinkTargetFor picks this dial's destination (ip, port). One counter walks the
// whole ip×port grid, so successive dials move across BOTH axes rather than
// exhausting one host's ports before touching the next.
func (c Config) sinkTargetFor() (string, int) {
	ips := c.SinkIPs
	if len(ips) == 0 {
		ips = []string{c.SinkIP}
	}
	nports := c.SinkPorts
	if nports < 1 {
		nports = 1
	}
	if c.dialSeq == nil || (len(ips) == 1 && nports == 1) {
		return ips[0], c.SinkPort
	}
	n := c.dialSeq.Add(1)
	return ips[int(n%uint64(len(ips)))], c.SinkPort + int((n/uint64(len(ips)))%uint64(nports))
}

// Spin burns d of CPU time in a busy-loop (it must compete for the core, so it
// is NOT a sleep — a sleep would yield and understate auth cost). DESIGN §3.1.
func Spin(d time.Duration) {
	if d <= 0 {
		return
	}
	end := time.Now().Add(d)
	for time.Now().Before(end) {
	}
}

// Decide runs the full hook for one connection and returns the connected upstream
// fd (raw, blocking, never via net/netpoller). The auth spin burns CPU; the dial
// park yields the goroutine (off-ring); the connect is a real blocking syscall.
func (c Config) Decide() (int, error) {
	Spin(c.AuthCPU)
	if c.Delay != nil {
		if d := c.Delay(); d > 0 {
			time.Sleep(d) // parks THIS goroutine (off-ring), not the ring worker
		}
	}
	ip, port := c.sinkTargetFor()
	return rawsock.DialMark(ip, port, c.Mark)
}
