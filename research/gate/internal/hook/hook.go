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
	Mark     int // SO_MARK on the upstream dial (fingerprint profile; 0 = none)
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
	return rawsock.DialMark(c.SinkIP, c.SinkPort, c.Mark)
}
