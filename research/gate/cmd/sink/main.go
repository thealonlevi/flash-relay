// Command sink is the gate's upstream server: reads exactly REQ_LEN request
// bytes (verifying the request pattern), writes REPLY_LEN reply bytes, closes.
// Infrastructure (own cores, never measured), so it may use net.
// See gate/DESIGN.md §1–§2.
package main

import (
	"flag"
	"log"
	"net"
	"strconv"

	"github.com/thealonlevi/flash-relay/research/gate/internal/proto"
	"github.com/thealonlevi/flash-relay/research/gate/internal/sinksrv"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9100", "listen address")
	reqLen := flag.Int("reqlen", proto.DefaultReqLen, "expected request bytes")
	replyLen := flag.Int("replylen", proto.DefaultReplyLen, "reply bytes to send")
	echo := flag.Bool("echo", false, "long-lived echo mode (for duplex/B3) instead of one-shot reply")
	statsFile := flag.String("statsfile", "", "if set, atomically write 'served=<n>' here every 250ms (optimizer two-fd anti-cheat)")
	ports := flag.Int("ports", 1, "listen on this many consecutive ports from -addr's port. Pair with relay-uring -sinkports: the relay dials upstream once per connection, and a single destination port confines every dial to ONE (srcIP,dstIP,dstPort) 4-tuple (~64k ephemeral ports), which caps the relay long before its CPU does.")
	flag.Parse()

	if *ports < 1 {
		log.Fatalf("-ports must be >= 1")
	}
	host, portStr, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Fatalf("addr %q: %v", *addr, err)
	}
	base, err := strconv.Atoi(portStr)
	if err != nil {
		log.Fatalf("addr %q: bad port: %v", *addr, err)
	}

	if *echo {
		if *ports != 1 {
			log.Fatalf("-echo supports a single port")
		}
		log.Fatal(sinksrv.ListenAndServeEcho(*addr, *statsFile))
	}

	// Bind the WHOLE span before serving anything. Binding lazily per goroutine
	// meant one unavailable port killed the process after other listeners were
	// already up, leaving the operator with a dead sink and a partial log. A
	// partially-bound sink is the worse failure anyway: the relay would dial a port
	// nobody listens on and the refused connections would read as relay errors.
	lns := make([]net.Listener, 0, *ports)
	for p := 0; p < *ports; p++ {
		a := net.JoinHostPort(host, strconv.Itoa(base+p))
		ln, err := net.Listen("tcp", a)
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			log.Fatalf("sink: bind %s failed: %v\n"+
				"  A stale connection or another process holds it. Check with:  ss -lntp | grep %d\n"+
				"  Ports in the span must also be reserved so the ephemeral allocator cannot take them:\n"+
				"    sysctl -w net.ipv4.ip_local_reserved_ports=%d-%d,...\n"+
				"  Reserving does NOT evict sockets already bound — wait for them to drain, or move the\n"+
				"  span with -addr.", a, err, base+p, base, base+*ports-1)
		}
		lns = append(lns, ln)
	}
	log.Fatal(sinksrv.ServeAll(lns, *reqLen, *replyLen, *statsFile))
}
