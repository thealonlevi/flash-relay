// Command relay-netpoll is the gate BASELINE: an equivalent Go relay on the
// standard net netpoller — accept → read initial request → decision hook
// (auth spin + dial park) → blocking-dial stub via net.Dial → io.Copy both
// ways. Everything goes through epoll/netpoller; B1 expects meaningful
// epoll_ctl/osq_lock/runtime_poll* CPU here. Same hook semantics as the SUT for
// a fair comparison. See gate/DESIGN.md §5.
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/thealonlevi/flash-relay/gate/internal/hook"
	"github.com/thealonlevi/flash-relay/gate/internal/proto"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "listen address")
	sink := flag.String("sink", "127.0.0.1:9100", "upstream sink address")
	reqLen := flag.Int("reqlen", proto.DefaultReqLen, "initial request bytes to read")
	authCPU := flag.Duration("authcpu", 5*time.Microsecond, "auth CPU busy-spin per conn")
	realistic := flag.Bool("realistic", false, "realistic-dial: sample ms-scale dial latency")
	dialP50 := flag.Float64("dialp50", 20, "realistic dial median ms")
	dialSigma := flag.Float64("dialsigma", 0.9, "realistic dial log-space sigma")
	dialCap := flag.Float64("dialcap", 30000, "realistic dial cap ms (dial timeout)")
	flag.Parse()

	var delay hook.DelayFunc = hook.NoDelay()
	if *realistic {
		delay = hook.Lognormal(*dialP50, *dialSigma, *dialCap, 1)
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("relay-netpoll (BASELINE) on %s -> sink %s (authcpu=%v realistic=%v)",
		*addr, *sink, *authCPU, *realistic)

	var completed, errs atomic.Uint64
	go func() {
		for range time.Tick(2 * time.Second) {
			log.Printf("baseline completed=%d errs=%d", completed.Load(), errs.Load())
		}
	}()

	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go func(client net.Conn) {
			defer client.Close()
			initial := make([]byte, *reqLen)
			if _, err := io.ReadFull(client, initial); err != nil {
				errs.Add(1)
				return
			}
			// Decision hook (off the accept loop, on this conn's goroutine).
			hook.Spin(*authCPU)
			if d := delay(); d > 0 {
				time.Sleep(d)
			}
			up, err := net.Dial("tcp", *sink) // netpoller dial — the baseline's cost
			if err != nil {
				errs.Add(1)
				return
			}
			defer up.Close()
			if _, err := up.Write(initial); err != nil {
				errs.Add(1)
				return
			}
			go io.Copy(up, client)   // client -> upstream (rest)
			io.Copy(client, up)      // upstream -> client (blocks to half-close)
			completed.Add(1)
		}(c)
	}
}
