// Command holdgen opens N concurrent LONG-LIVED relayed connections and holds
// them open (light keepalive traffic), modeling the ISP incident's failure mode:
// huge concurrent held connections, not connect-rate churn. This is where the
// netpoller's goroutine-per-connection model collapses (scheduler thrash + GC
// over hundreds of thousands of goroutine stacks) and flash-relay (one ring
// worker, connections as map entries) does not. Infrastructure — may use net.
//
// Pair with: relay in -duplex mode + sink in -echo mode.
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/thealonlevi/flash-relay/research/gate/internal/proto"
)

func main() {
	relay := flag.String("relay", "127.0.0.1:21000", "relay address")
	n := flag.Int("n", 100000, "concurrent held connections")
	reqLen := flag.Int("reqlen", proto.DefaultReqLen, "initial request bytes (echoed back via the relay)")
	ramp := flag.Duration("ramp", 12*time.Second, "spread connection establishment over this window")
	hold := flag.Duration("hold", 25*time.Second, "hold connections open this long after establishing")
	keepalive := flag.Duration("keepalive", 2*time.Second, "send a small keepalive every interval (keeps conns active)")
	kbytes := flag.Int("kbytes", 16, "keepalive payload bytes")
	flag.Parse()

	req := proto.Request(*reqLen)
	ka := make([]byte, *kbytes)
	var established, errs, holding atomic.Int64
	stagger := time.Duration(0)
	if *n > 0 {
		stagger = *ramp / time.Duration(*n)
	}
	deadline := time.Now().Add(*ramp + *hold)

	go func() {
		for range time.Tick(2 * time.Second) {
			log.Printf("holdgen established=%d holding=%d errs=%d", established.Load(), holding.Load(), errs.Load())
		}
	}()

	for i := 0; i < *n; i++ {
		go func(i int) {
			time.Sleep(time.Duration(i) * stagger) // spread the ramp
			c, err := net.DialTimeout("tcp", *relay, 10*time.Second)
			if err != nil {
				errs.Add(1)
				return
			}
			defer c.Close()
			if _, err := c.Write(req); err != nil {
				errs.Add(1)
				return
			}
			buf := make([]byte, *reqLen)
			c.SetReadDeadline(time.Now().Add(15 * time.Second))
			if _, err := io.ReadFull(c, buf); err != nil { // echo of the initial request
				errs.Add(1)
				return
			}
			established.Add(1)
			holding.Add(1)
			defer holding.Add(-1)
			kb := make([]byte, *kbytes)
			for time.Now().Before(deadline) {
				time.Sleep(*keepalive)
				c.SetDeadline(time.Now().Add(15 * time.Second))
				if _, err := c.Write(ka); err != nil {
					return
				}
				if _, err := io.ReadFull(c, kb); err != nil {
					return
				}
			}
		}(i)
	}

	time.Sleep(time.Until(deadline) + 500*time.Millisecond)
	log.Printf("DONE established=%d peak-holding=%d errs=%d", established.Load(), *n-int(errs.Load()), errs.Load())
}
