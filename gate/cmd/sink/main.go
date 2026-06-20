// Command sink is the gate's upstream server: reads exactly REQ_LEN request
// bytes (verifying the request pattern), writes REPLY_LEN reply bytes, closes.
// Infrastructure (own cores, never measured), so it may use net.
// See gate/DESIGN.md §1–§2.
package main

import (
	"flag"
	"log"

	"github.com/thealonlevi/flash-relay/gate/internal/proto"
	"github.com/thealonlevi/flash-relay/gate/internal/sinksrv"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9100", "listen address")
	reqLen := flag.Int("reqlen", proto.DefaultReqLen, "expected request bytes")
	replyLen := flag.Int("replylen", proto.DefaultReplyLen, "reply bytes to send")
	flag.Parse()

	log.Fatal(sinksrv.ListenAndServe(*addr, *reqLen, *replyLen))
}
