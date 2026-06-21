//go:build linux

// Command bulkgen is the B4 bulk-throughput generator: it holds N full-duplex
// connections open through the relay (which must run -duplex against an -echo
// sink) and streams a known pattern through each, counting bytes that come back
// VERIFIED. That verified-round-trip count is the anti-cheat-safe throughput
// numerator for the bytes_per_cpu objective: the optimizer cannot inflate it by
// dropping/short-circuiting data, because every counted byte was echoed back
// intact. Infrastructure (never the SUT), so it may use net. Pairs with the
// churn loadgen; this one stresses the data plane (recv/send/splice), not accept.
package main

import (
	"encoding/json"
	"flag"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// pattern returns a deterministic, position-independent byte stream so a reader
// can verify any slice of it knowing only the absolute offset (offset mod 251).
func patByte(off uint64) byte { return byte(off % 251) }

type result struct {
	Relay       string  `json:"relay"`
	Conns       int     `json:"conns"`
	ChunkBytes  int     `json:"chunk_bytes"`
	DurationSec float64 `json:"duration_sec"`
	Bytes       uint64  `json:"bytes"` // verified round-trip payload bytes
	BytesPerSec float64 `json:"bytes_per_sec"`
	AuditFail   uint64  `json:"audit_fail"`
	Errors      uint64  `json:"errors"`
}

func main() {
	relay := flag.String("relay", "127.0.0.1:9000", "relay address (relay must be -duplex -> -echo sink)")
	conns := flag.Int("conns", 64, "concurrent full-duplex connections")
	chunk := flag.Int("chunk", 262144, "write/read chunk bytes (256 KiB default)")
	dur := flag.Duration("duration", 10*time.Second, "measurement window")
	warmup := flag.Duration("warmup", 2*time.Second, "warmup before timing")
	flag.Parse()

	var verified, auditFail, errs atomic.Uint64
	var measuring atomic.Bool
	stop := make(chan struct{})

	// One buffer of the repeating pattern, reused by every writer.
	out := make([]byte, *chunk)
	for i := range out {
		out[i] = patByte(uint64(i))
	}

	var wg sync.WaitGroup
	for w := 0; w < *conns; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var d net.Dialer
			d.Timeout = 3 * time.Second
			c, err := d.Dial("tcp", *relay)
			if err != nil {
				errs.Add(1)
				return
			}
			defer c.Close()

			// Writer goroutine: stream the pattern continuously (true full-duplex
			// so a full socket buffer can never deadlock against our own reader).
			done := make(chan struct{})
			go func() {
				var woff uint64
				wb := make([]byte, *chunk)
				for {
					select {
					case <-stop:
						close(done)
						return
					default:
					}
					for i := range wb {
						wb[i] = patByte(woff + uint64(i))
					}
					c.SetWriteDeadline(time.Now().Add(10 * time.Second))
					n, err := c.Write(wb)
					if err != nil {
						close(done)
						return
					}
					woff += uint64(n)
				}
			}()

			// Reader: verify the echoed stream against the pattern at its offset.
			in := make([]byte, *chunk)
			var roff uint64
			for {
				select {
				case <-stop:
					return
				case <-done:
					return
				default:
				}
				c.SetReadDeadline(time.Now().Add(10 * time.Second))
				n, err := c.Read(in)
				if n > 0 {
					bad := false
					for i := 0; i < n; i++ {
						if in[i] != patByte(roff+uint64(i)) {
							bad = true
							break
						}
					}
					if bad {
						auditFail.Add(1)
						return
					}
					roff += uint64(n)
					if measuring.Load() {
						verified.Add(uint64(n))
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}

	time.Sleep(*warmup)
	measuring.Store(true)
	start := time.Now()
	time.Sleep(*dur)
	elapsed := time.Since(start)
	measuring.Store(false)
	close(stop)
	wg.Wait()

	b := verified.Load()
	json.NewEncoder(os.Stdout).Encode(result{
		Relay: *relay, Conns: *conns, ChunkBytes: *chunk, DurationSec: elapsed.Seconds(),
		Bytes: b, BytesPerSec: float64(b) / elapsed.Seconds(),
		AuditFail: auditFail.Load(), Errors: errs.Load(),
	})
}
