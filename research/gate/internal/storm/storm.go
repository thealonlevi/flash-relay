//go:build linux

// Package storm is the gate's client connection-storm + byte-audit + latency
// sampler, factored out so both the one-shot `loadgen` CLI and the `loadgend`
// control daemon share one implementation. Infrastructure (never the SUT), so it
// may use net. See gate/DESIGN.md §2,§6.
package storm

import (
	"fmt"
	"io"
	"math/rand"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thealonlevi/flash-relay/research/gate/internal/proto"
)

// Config is one storm run.
type Config struct {
	// Relay is the target, either "ip:port" or a port SPAN "ip:18000-18015".
	// A span matters when this box has few source IPs: one
	// (srcIP,dstIP,dstPort) 4-tuple caps near ~64k ephemeral ports, so a
	// single-IP loadgen tops out around that rate no matter how many cores it
	// has. Each extra destination port buys another full port space, which is
	// the only lever available when extra source IPs are not.
	Relay    string
	ReqLen   int
	ReplyLen int
	InFlight int
	Warmup   time.Duration
	Duration time.Duration
	JunkPct  int // % of connections that are zero-byte connect-flood junk (connect→close, no request, never reaches upstream). Models the ISP connect-flood incident.
	// SrcIPs, if non-empty, are local source IPs to bind connections to,
	// assigned round-robin across workers. This multiplies the usable
	// ephemeral-port space — one (srcIP,dstIP,dstPort) 4-tuple caps near ~64k
	// source ports, so spreading across N source IPs gives ~N×. Empty = let the
	// kernel pick the route's default source (a single IP). See ResolveSrcIPs.
	SrcIPs []string
	// Cancel, if non-nil, cuts the run short when closed. Without it a storm that
	// wedges (e.g. a huge in-flight count against a lossy path) holds the daemon's
	// one-storm-at-a-time lock with no remote way to clear it, which strands a
	// multi-point sweep that has to fire many storms in sequence.
	Cancel <-chan struct{}
}

// Result is the measured outcome (JSON-tagged to match the loadgen output that
// combine-2box.py consumes).
type Result struct {
	Relay       string   `json:"relay"`
	InFlight    int      `json:"in_flight"`
	ReqLen      int      `json:"req_len"`
	ReplyLen    int      `json:"reply_len"`
	DurationSec float64  `json:"duration_sec"`
	Completed   uint64   `json:"completed"`
	Junk        uint64   `json:"junk"`
	Errors      uint64   `json:"errors"`
	AuditFail   uint64   `json:"audit_fail"`
	ConnPerSec  float64  `json:"conn_per_sec"`
	P50us       float64  `json:"p50_us"`
	P99us       float64  `json:"p99_us"`
	P999us      float64  `json:"p999_us"`
	Samples     int      `json:"latency_samples"`
	SrcIPs      []string `json:"src_ips,omitempty"`
}

// Run holds Config.InFlight relayed connections in flight for Warmup+Duration,
// each: dial relay → write REQUEST → read exactly ReplyLen → verify pattern →
// close, timing connect-to-first-reply-byte. Blocks until done.
func Run(cfg Config) Result {
	req := proto.Request(cfg.ReqLen)
	wantReply := proto.Reply(cfg.ReplyLen)

	targets, err := ExpandTargets(cfg.Relay)
	if err != nil || len(targets) == 0 {
		return Result{Relay: cfg.Relay, Errors: 1}
	}

	var completed, junk, errs, auditFail atomic.Uint64
	var measuring atomic.Bool
	stop := make(chan struct{})

	// Precompute the local source addresses (port 0 = kernel picks the source
	// port within that IP). Empty list => one nil dialer, kernel-default source.
	// Keep only source IPs of the relay's address family — binding a v6 source
	// to a v4 relay (or vice versa) fails every dial.
	relayIsV4 := true
	if ra, err := net.ResolveTCPAddr("tcp", targets[0]); err == nil && ra.IP != nil {
		relayIsV4 = ra.IP.To4() != nil
	}
	var laddrs []*net.TCPAddr
	for _, ip := range cfg.SrcIPs {
		pip := net.ParseIP(ip)
		if pip == nil || (pip.To4() != nil) != relayIsV4 {
			continue
		}
		laddrs = append(laddrs, &net.TCPAddr{IP: pip})
	}

	lat := make([][]int64, cfg.InFlight) // per-worker, merged at end (no contention)
	var wg sync.WaitGroup
	for w := 0; w < cfg.InFlight; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// Each worker pins to one source IP (round-robin by index) so
			// concurrency spreads evenly across the IPs' port spaces.
			var dialer net.Dialer
			dialer.Timeout = 3 * time.Second // never hang on a stalled/overwhelmed relay
			if len(laddrs) > 0 {
				dialer.LocalAddr = laddrs[w%len(laddrs)]
			}
			// Spread workers over the destination ports so the ephemeral-port
			// space actually used is len(targets) x 64k rather than one port's.
			target := targets[w%len(targets)]
			buf := make([]byte, cfg.ReplyLen)
			rng := rand.New(rand.NewSource(int64(w)*2654435761 + 1))
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Junk: zero-byte connect-flood — connect then close, no request,
				// never reaches upstream. Models the 93%-junk ISP incident.
				if cfg.JunkPct > 0 && rng.Intn(100) < cfg.JunkPct {
					c, err := dialer.Dial("tcp", target) // source-IP + dst-port spread, timeout
					if err != nil {
						errs.Add(1)
						continue
					}
					c.Close()
					if measuring.Load() {
						junk.Add(1)
					}
					continue
				}
				t0 := time.Now()
				c, err := dialer.Dial("tcp", target)
				if err != nil {
					errs.Add(1)
					continue
				}
				// Deadline so a stalled/overwhelmed relay yields a counted error,
				// never a hung worker (the harness must report degradation).
				c.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := c.Write(req); err != nil {
					errs.Add(1)
					c.Close()
					continue
				}
				if _, err := io.ReadFull(c, buf); err != nil {
					errs.Add(1)
					c.Close()
					continue
				}
				elapsed := time.Since(t0).Microseconds()
				c.Close()
				if !proto.Equal(buf, wantReply) {
					auditFail.Add(1)
					continue
				}
				if measuring.Load() {
					completed.Add(1)
					lat[w] = append(lat[w], elapsed)
				}
			}
		}(w)
	}

	// Warmup and the measured window are both cancellable, so /stop takes effect
	// promptly instead of after the full requested duration.
	sleepOrCancel := func(d time.Duration) {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-cfg.Cancel:
		}
	}
	sleepOrCancel(cfg.Warmup)
	measuring.Store(true)
	start := time.Now()
	sleepOrCancel(cfg.Duration)
	elapsed := time.Since(start)
	measuring.Store(false)
	close(stop)
	wg.Wait()

	var all []int64
	for _, s := range lat {
		all = append(all, s...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

	return Result{
		Relay: cfg.Relay, InFlight: cfg.InFlight, ReqLen: cfg.ReqLen, ReplyLen: cfg.ReplyLen,
		DurationSec: elapsed.Seconds(),
		Completed:   completed.Load(), Junk: junk.Load(), Errors: errs.Load(), AuditFail: auditFail.Load(),
		ConnPerSec: float64(completed.Load()+junk.Load()) / elapsed.Seconds(),
		P50us:      pct(all, 0.50), P99us: pct(all, 0.99), P999us: pct(all, 0.999),
		Samples: len(all),
		SrcIPs:  cfg.SrcIPs,
	}
}

// ExpandTargets expands a relay spec into concrete "ip:port" dial targets:
//
//	"10.0.0.1:18000"        -> ["10.0.0.1:18000"]
//	"10.0.0.1:18000-18003"  -> ["10.0.0.1:18000" ... "10.0.0.1:18003"]
//
// The span form exists to multiply the client's usable ephemeral-port space when
// extra source IPs are not available; see Config.Relay.
func ExpandTargets(spec string) ([]string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("empty relay target")
	}
	host, portPart, err := net.SplitHostPort(spec)
	if err != nil {
		return nil, fmt.Errorf("relay %q: %w", spec, err)
	}
	lo, hi := portPart, portPart
	if i := strings.IndexByte(portPart, '-'); i >= 0 {
		lo, hi = portPart[:i], portPart[i+1:]
	}
	a, err := strconv.Atoi(lo)
	if err != nil {
		return nil, fmt.Errorf("relay %q: bad port %q", spec, lo)
	}
	b, err := strconv.Atoi(hi)
	if err != nil {
		return nil, fmt.Errorf("relay %q: bad port %q", spec, hi)
	}
	if b < a {
		return nil, fmt.Errorf("relay %q: port span end %d is below start %d", spec, b, a)
	}
	var out []string
	for p := a; p <= b; p++ {
		out = append(out, net.JoinHostPort(host, strconv.Itoa(p)))
	}
	return out, nil
}

// ResolveSrcIPs expands a source-IP spec into concrete local IPs to bind to:
//
//	""      -> nil  (kernel picks the route's default source — a single IP)
//	"auto"  -> every global-unicast IP on the host's interfaces
//	csv     -> the listed IPs, validated verbatim
//
// "auto" is the "use all routable IPs here" option: a box with N assigned
// public IPs gets ~N× the ephemeral-port headroom.
func ResolveSrcIPs(spec string) ([]string, error) {
	spec = strings.TrimSpace(spec)
	switch spec {
	case "":
		return nil, nil
	case "auto":
		return localGlobalIPs()
	}
	var out []string
	for _, p := range strings.Split(spec, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if net.ParseIP(p) == nil {
			return nil, fmt.Errorf("invalid source IP %q", p)
		}
		out = append(out, p)
	}
	return out, nil
}

// localGlobalIPs returns every global-unicast IP assigned to the host's
// interfaces (excludes loopback, link-local, multicast). Includes private LAN
// addresses, which are legitimate sources on a bench network.
func localGlobalIPs() ([]string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip == nil || !ip.IsGlobalUnicast() {
			continue
		}
		out = append(out, ip.String())
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("no global-unicast IPs found on host")
	}
	return out, nil
}

func pct(sorted []int64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return float64(sorted[i])
}
