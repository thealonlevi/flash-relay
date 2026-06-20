// Command loadgen is the gate's one-shot client connection-storm + byte-audit +
// latency sampler. It holds a fixed number of in-flight relayed connections,
// each: dial relay → write REQUEST → read exactly REPLY_LEN → verify pattern →
// close, timing connect-to-first-reply-byte. Emits a JSON result. Infrastructure
// (own cores, never measured), so it may use net. See gate/DESIGN.md §2,§6.
//
// For driving the storm remotely from the SUT box, see cmd/loadgend.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"os"
	"time"

	"github.com/thealonlevi/flash-relay/gate/internal/proto"
	"github.com/thealonlevi/flash-relay/gate/internal/storm"
)

func main() {
	relay := flag.String("relay", "127.0.0.1:9000", "relay address")
	reqLen := flag.Int("reqlen", proto.DefaultReqLen, "request bytes")
	replyLen := flag.Int("replylen", proto.DefaultReplyLen, "expected reply bytes")
	inflight := flag.Int("inflight", 512, "concurrent in-flight connections")
	dur := flag.Duration("duration", 10*time.Second, "measurement window")
	warmup := flag.Duration("warmup", 2*time.Second, "warmup before timing")
	flag.Parse()

	res := storm.Run(storm.Config{
		Relay: *relay, ReqLen: *reqLen, ReplyLen: *replyLen,
		InFlight: *inflight, Warmup: *warmup, Duration: *dur,
	})
	if res.AuditFail > 0 {
		log.Printf("WARNING: %d byte-audit failures — run is INVALID", res.AuditFail)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(res)
}
