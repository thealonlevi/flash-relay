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
	"strings"

	"github.com/thealonlevi/flash-relay/research/gate/internal/proto"
	"github.com/thealonlevi/flash-relay/research/gate/internal/sinksrv"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9100", "listen address")
	reqLen := flag.Int("reqlen", proto.DefaultReqLen, "expected request bytes")
	replyLen := flag.Int("replylen", proto.DefaultReplyLen, "reply bytes to send")
	echo := flag.Bool("echo", false, "long-lived echo mode (for duplex/B3) instead of one-shot reply")
	statsFile := flag.String("statsfile", "", "if set, atomically write 'served=<n>' here every 250ms (optimizer two-fd anti-cheat)")
	window := flag.Int("portwindow", 512, "scan this many ports from -addr's base to find -ports free ones. They need not be contiguous: on a box also running a busy proxy the ephemeral allocator churns ports across the whole range, so no fixed span is reliably free. The bound set is printed as SINK_PORTS_BOUND=.")
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
	// SCAN for free ports rather than demanding a contiguous span. On a box that is
	// also running a busy proxy, the ephemeral allocator is constantly churning
	// ports across the whole range (and prefers ODD ones for connect()), so no fixed
	// contiguous span is reliably free — every attempt collides with whatever that
	// proxy happens to hold right now. The ports need not be contiguous; they only
	// need to be known, so the chosen set is printed for the relay to dial.
	lns := make([]net.Listener, 0, *ports)
	chosen := make([]string, 0, *ports)
	for p := base; p < base+*window && len(lns) < *ports; p++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(p)))
		if err != nil {
			continue // busy: skip it, keep scanning
		}
		lns = append(lns, ln)
		chosen = append(chosen, strconv.Itoa(p))
	}
	if len(lns) < *ports {
		for _, l := range lns {
			l.Close()
		}
		log.Fatalf("sink: found only %d free ports of %d wanted in %d-%d.\n"+
			"  Raise -portwindow, move -addr's base port, or reduce -ports.",
			len(lns), *ports, base, base+*window-1)
	}
	// Machine-readable so the setup script can hand the relay an exact target list.
	log.Printf("SINK_PORTS_BOUND=%s", strings.Join(chosen, ","))
	log.Fatal(sinksrv.ServeAll(lns, *reqLen, *replyLen, *statsFile))
}
