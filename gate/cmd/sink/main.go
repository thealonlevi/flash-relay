// Command sink is the gate's upstream server: it reads exactly REQ_LEN request
// bytes (verifying the request pattern), writes REPLY_LEN reply bytes, and
// closes. It is infrastructure (runs on its own cores, never measured), so it
// may use the net package. See gate/DESIGN.md §1–§2.
package main

import (
	"flag"
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/thealonlevi/flash-relay/gate/internal/proto"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9100", "listen address")
	reqLen := flag.Int("reqlen", proto.DefaultReqLen, "expected request bytes")
	replyLen := flag.Int("replylen", proto.DefaultReplyLen, "reply bytes to send")
	flag.Parse()

	want := proto.Request(*reqLen)
	reply := proto.Reply(*replyLen)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("sink listening on %s (reqlen=%d replylen=%d)", *addr, *reqLen, *replyLen)

	var served, auditFail, errs atomic.Uint64
	go func() {
		for range time.Tick(2 * time.Second) {
			log.Printf("sink served=%d auditFail=%d errs=%d",
				served.Load(), auditFail.Load(), errs.Load())
		}
	}()

	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			buf := make([]byte, len(want))
			if _, err := io.ReadFull(c, buf); err != nil {
				errs.Add(1)
				return
			}
			if !proto.Equal(buf, want) {
				auditFail.Add(1) // relay forwarded the wrong request bytes
			}
			if _, err := c.Write(reply); err != nil {
				errs.Add(1)
				return
			}
			served.Add(1)
		}(c)
	}
}
