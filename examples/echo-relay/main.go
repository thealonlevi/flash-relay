//go:build linux && amd64

// Command echo-relay is a minimal example of embedding the flashrelay library:
// a TCP relay that authorizes each connection in a Hook and forwards it to a
// fixed upstream. Run an upstream (e.g. `nc -lk 9000`) and then:
//
//	go run ./examples/echo-relay -addr 127.0.0.1 -port 8080 -upstream 127.0.0.1:9000
//
// Connect to :8080; bytes are relayed to the upstream and back, with NO Go
// netpoller on any data-plane fd. Ctrl-C drains in-flight connections cleanly.
package main

import (
	"flag"
	"log"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thealonlevi/flash-relay/flashrelay"
)

func main() {
	addr := flag.String("addr", "127.0.0.1", "bind IP (IPv4 or IPv6; \"\" = all)")
	port := flag.Int("port", 8080, "listen port")
	upstream := flag.String("upstream", "127.0.0.1:9000", "upstream host:port to relay to")
	workers := flag.Int("workers", 1, "per-core rings (SO_REUSEPORT)")
	flag.Parse()

	upHost, upPort := splitHostPort(*upstream)

	// The Hook runs off-ring. Here it just dials the fixed upstream with a
	// blocking raw syscall (so the upstream fd never touches the netpoller) and
	// hands the fd to the engine. A real deployment would authenticate, look up
	// a blacklist, allocate an egress IP, etc. — all on this off-ring goroutine.
	hook := func(req []byte, peer netip.AddrPort) flashrelay.Decision {
		fd, err := flashrelay.Dial(upHost, upPort)
		if err != nil {
			log.Printf("dial upstream for %s: %v", peer, err)
			return flashrelay.Decision{Reject: true, Reply: []byte("upstream unavailable\n")}
		}
		return flashrelay.Decision{UpstreamFD: fd}
	}

	srv, err := flashrelay.New(flashrelay.Config{
		Addr:        *addr,
		Port:        *port,
		Workers:     *workers,
		IdleTimeout: 5 * time.Minute,
	}, hook)
	if err != nil {
		log.Fatalf("flashrelay.New: %v", err)
	}

	// Ctrl-C -> graceful drain.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Printf("draining…")
		srv.Stop()
	}()

	log.Printf("relay on %s:%d -> %s (%d worker(s))", *addr, *port, *upstream, *workers)
	if err := srv.Run(); err != nil {
		log.Fatalf("run: %v", err)
	}
	st := srv.Stat()
	log.Printf("done: accepted=%d completed=%d rejected=%d bytesC2U=%d bytesU2C=%d",
		st.Accepted, st.Completed, st.Rejected, st.BytesC2U, st.BytesU2C)
}

func splitHostPort(hp string) (string, int) {
	i := strings.LastIndexByte(hp, ':')
	if i < 0 {
		log.Fatalf("upstream %q must be host:port", hp)
	}
	p, err := strconv.Atoi(hp[i+1:])
	if err != nil {
		log.Fatalf("upstream port: %v", err)
	}
	return hp[:i], p
}
