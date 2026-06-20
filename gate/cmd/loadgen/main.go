// Command loadgen is the gate's client connection-storm + byte-audit + latency
// sampler. It holds a fixed number of in-flight relayed connections, each:
// dial relay → write REQUEST → read exactly REPLY_LEN → verify pattern → close,
// timing connect-to-first-reply-byte. It is infrastructure (own cores, never
// measured), so it may use net. Emits a JSON result. See gate/DESIGN.md §2,§6.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thealonlevi/flash-relay/gate/internal/proto"
)

type result struct {
	Relay        string  `json:"relay"`
	InFlight     int     `json:"in_flight"`
	ReqLen       int     `json:"req_len"`
	ReplyLen     int     `json:"reply_len"`
	DurationSec  float64 `json:"duration_sec"`
	Completed    uint64  `json:"completed"`
	Errors       uint64  `json:"errors"`
	AuditFail    uint64  `json:"audit_fail"`
	ConnPerSec   float64 `json:"conn_per_sec"`
	P50us        float64 `json:"p50_us"`
	P99us        float64 `json:"p99_us"`
	P999us       float64 `json:"p999_us"`
	Samples      int     `json:"latency_samples"`
}

func main() {
	relay := flag.String("relay", "127.0.0.1:9000", "relay address")
	reqLen := flag.Int("reqlen", proto.DefaultReqLen, "request bytes")
	replyLen := flag.Int("replylen", proto.DefaultReplyLen, "expected reply bytes")
	inflight := flag.Int("inflight", 512, "concurrent in-flight connections")
	dur := flag.Duration("duration", 10*time.Second, "measurement window")
	warmup := flag.Duration("warmup", 2*time.Second, "warmup before timing")
	flag.Parse()

	req := proto.Request(*reqLen)
	wantReply := proto.Reply(*replyLen)

	var completed, errs, auditFail atomic.Uint64
	var measuring atomic.Bool
	stop := make(chan struct{})

	// Each worker keeps its own latency slice to avoid contention; merged at end.
	lat := make([][]int64, *inflight)
	var wg sync.WaitGroup
	for w := 0; w < *inflight; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			buf := make([]byte, *replyLen)
			for {
				select {
				case <-stop:
					return
				default:
				}
				t0 := time.Now()
				c, err := net.Dial("tcp", *relay)
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

	time.Sleep(*warmup)
	measuring.Store(true)
	start := time.Now()
	time.Sleep(*dur)
	elapsed := time.Since(start)
	measuring.Store(false)
	close(stop)
	wg.Wait()

	var all []int64
	for _, s := range lat {
		all = append(all, s...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

	res := result{
		Relay: *relay, InFlight: *inflight, ReqLen: *reqLen, ReplyLen: *replyLen,
		DurationSec: elapsed.Seconds(),
		Completed:   completed.Load(), Errors: errs.Load(), AuditFail: auditFail.Load(),
		ConnPerSec: float64(completed.Load()) / elapsed.Seconds(),
		P50us:      pct(all, 0.50), P99us: pct(all, 0.99), P999us: pct(all, 0.999),
		Samples: len(all),
	}
	if res.AuditFail > 0 {
		log.Printf("WARNING: %d byte-audit failures — run is INVALID", res.AuditFail)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(res)
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
