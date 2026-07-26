// Command relay-netpoll is the gate BASELINE: an equivalent Go relay on the
// standard net netpoller — accept → read initial request → decision hook
// (auth spin + dial park) → blocking-dial stub via net.Dial → io.Copy both
// ways. Everything goes through epoll/netpoller; B1 expects meaningful
// epoll_ctl/osq_lock/runtime_poll* CPU here. Same hook semantics as the SUT for
// a fair comparison. See gate/DESIGN.md §5.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thealonlevi/flash-relay/research/gate/internal/hook"
	"github.com/thealonlevi/flash-relay/research/gate/internal/proto"
)

// writeStat mirrors relay-uring's format: under the connect-flood profile most
// connections are accepted but never complete, so both builds must publish both
// counters or the two sides of the comparison measure different things.
func writeStat(path string, completed, accepted uint64) {
	tmp := path + ".tmp"
	body := fmt.Sprintf("completed=%d\naccepted=%d\n", completed, accepted)
	if os.WriteFile(tmp, []byte(body), 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9000", "listen address")
	sink := flag.String("sink", "127.0.0.1:9100", "upstream sink address")
	reqLen := flag.Int("reqlen", proto.DefaultReqLen, "initial request bytes to read")
	authCPU := flag.Duration("authcpu", 5*time.Microsecond, "auth CPU busy-spin per conn")
	realistic := flag.Bool("realistic", false, "realistic-dial: sample ms-scale dial latency")
	dialP50 := flag.Float64("dialp50", 20, "realistic dial median ms")
	dialSigma := flag.Float64("dialsigma", 0.9, "realistic dial log-space sigma")
	dialCap := flag.Float64("dialcap", 30000, "realistic dial cap ms (dial timeout)")
	statsFile := flag.String("statsfile", "", "if set, atomically write the 'completed=' and 'accepted=' counters here every 250ms (2-box harness)")
	nPorts := flag.Int("ports", 1, "listen on this many consecutive ports starting at -addr's port. Mirrors relay-uring's -ports so both builds face the SAME client port-space; the baseline must not be handed a narrower 4-tuple space than the SUT or the comparison is rigged.")
	flag.Parse()

	var delay hook.DelayFunc = hook.NoDelay()
	if *realistic {
		delay = hook.Lognormal(*dialP50, *dialSigma, *dialCap, 1)
	}

	if *nPorts < 1 {
		log.Fatalf("-ports must be >= 1")
	}
	host, portStr, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Fatalf("addr %q: %v", *addr, err)
	}
	basePort, err := strconv.Atoi(portStr)
	if err != nil {
		log.Fatalf("addr %q: bad port: %v", *addr, err)
	}
	lns := make([]net.Listener, 0, *nPorts)
	for p := 0; p < *nPorts; p++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(basePort+p)))
		if err != nil {
			log.Fatalf("listen %d: %v", basePort+p, err)
		}
		lns = append(lns, ln)
	}
	log.Printf("relay-netpoll (BASELINE) on %s ports %d..%d -> sink %s (authcpu=%v realistic=%v)",
		host, basePort, basePort+*nPorts-1, *sink, *authCPU, *realistic)

	var completed, errs, accepted atomic.Uint64
	go func() {
		for range time.Tick(2 * time.Second) {
			log.Printf("baseline completed=%d errs=%d", completed.Load(), errs.Load())
		}
	}()
	if *statsFile != "" {
		go func() {
			for range time.Tick(250 * time.Millisecond) {
				writeStat(*statsFile, completed.Load(), accepted.Load())
			}
		}()
	}

	// One accept loop per port. They all feed the same shared Go scheduler and
	// netpoller, which is exactly the baseline property under test.
	var wg sync.WaitGroup
	for _, ln := range lns {
		wg.Add(1)
		go func(ln net.Listener) {
			defer wg.Done()
			acceptLoop(ln, sink, reqLen, authCPU, delay, &completed, &errs, &accepted)
		}(ln)
	}
	wg.Wait()
}

func acceptLoop(ln net.Listener, sink *string, reqLen *int, authCPU *time.Duration,
	delay hook.DelayFunc, completed, errs, accepted *atomic.Uint64) {
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		accepted.Add(1)
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
