//go:build linux

// Package storm is the gate's client connection-storm + byte-audit + latency
// sampler, factored out so both the one-shot `loadgen` CLI and the `loadgend`
// control daemon share one implementation. Infrastructure (never the SUT), so it
// may use net. See gate/DESIGN.md §2,§6.
package storm

import (
	"io"
	"math/rand"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thealonlevi/flash-relay/gate/internal/proto"
)

// Config is one storm run.
type Config struct {
	Relay    string
	ReqLen   int
	ReplyLen int
	InFlight int
	Warmup   time.Duration
	Duration time.Duration
	JunkPct  int // % of connections that are zero-byte connect-flood junk (connect→close, no request, never reaches upstream). Models the ISP connect-flood incident.
}

// Result is the measured outcome (JSON-tagged to match the loadgen output that
// combine-2box.py consumes).
type Result struct {
	Relay       string  `json:"relay"`
	InFlight    int     `json:"in_flight"`
	ReqLen      int     `json:"req_len"`
	ReplyLen    int     `json:"reply_len"`
	DurationSec float64 `json:"duration_sec"`
	Completed   uint64  `json:"completed"`
	Junk        uint64  `json:"junk"`
	Errors      uint64  `json:"errors"`
	AuditFail   uint64  `json:"audit_fail"`
	ConnPerSec  float64 `json:"conn_per_sec"`
	P50us       float64 `json:"p50_us"`
	P99us       float64 `json:"p99_us"`
	P999us      float64 `json:"p999_us"`
	Samples     int     `json:"latency_samples"`
}

// Run holds Config.InFlight relayed connections in flight for Warmup+Duration,
// each: dial relay → write REQUEST → read exactly ReplyLen → verify pattern →
// close, timing connect-to-first-reply-byte. Blocks until done.
func Run(cfg Config) Result {
	req := proto.Request(cfg.ReqLen)
	wantReply := proto.Reply(cfg.ReplyLen)

	var completed, junk, errs, auditFail atomic.Uint64
	var measuring atomic.Bool
	stop := make(chan struct{})

	lat := make([][]int64, cfg.InFlight) // per-worker, merged at end (no contention)
	var wg sync.WaitGroup
	for w := 0; w < cfg.InFlight; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
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
					c, err := net.Dial("tcp", cfg.Relay)
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
				c, err := net.Dial("tcp", cfg.Relay)
				if err != nil {
					errs.Add(1)
					continue
				}
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

	time.Sleep(cfg.Warmup)
	measuring.Store(true)
	start := time.Now()
	time.Sleep(cfg.Duration)
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
		Samples:    len(all),
	}
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
