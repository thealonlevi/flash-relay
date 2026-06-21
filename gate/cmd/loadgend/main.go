// Command loadgend is the gate's loadgen CONTROL DAEMON for the loadgen box
// (box 2). It lets the SUT box (box 1) drive the whole 2-box run remotely with
// curl — no SSH, no manual coordination:
//
//	loadgend -control 0.0.0.0:9200 -sink 0.0.0.0:9100 -relay <BOX1_IP>:18000
//
// then from box 1:
//
//	curl -s "http://<BOX2_IP>:9200/run?inflight=512&duration=90s&warmup=5s" > uring_loadgen.json
//
// It optionally hosts the sink in-process (so box 2 is one command) and runs one
// storm at a time. Infrastructure (never the SUT) — uses net/http freely.
//
// SECURITY: this exposes a load-trigger endpoint. Bind it to the bench network
// and firewall the control port to the SUT box only. Bench use only.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/thealonlevi/flash-relay/gate/internal/proto"
	"github.com/thealonlevi/flash-relay/gate/internal/sinksrv"
	"github.com/thealonlevi/flash-relay/gate/internal/storm"
)

func main() {
	control := flag.String("control", "0.0.0.0:9200", "control HTTP listen address")
	sinkAddr := flag.String("sink", "0.0.0.0:9100", "run the sink in-process on this address (empty to disable)")
	relay := flag.String("relay", "", "default relay target <ip>:<port> (overridable per request)")
	reqLen := flag.Int("reqlen", proto.DefaultReqLen, "default request bytes")
	replyLen := flag.Int("replylen", proto.DefaultReplyLen, "default reply bytes")
	srcSpec := flag.String("srcips", "auto", `source IPs to spread the storm across: "auto" (all global IPs on this box), "" (kernel default), or a csv list; overridable per request via ?srcips=`)
	flag.Parse()

	srcIPs, err := storm.ResolveSrcIPs(*srcSpec)
	if err != nil {
		log.Fatalf("srcips: %v", err)
	}

	if *sinkAddr != "" {
		go func() { log.Fatalf("sink: %v", sinksrv.ListenAndServe(*sinkAddr, *reqLen, *replyLen, "")) }()
	}

	var busy sync.Mutex // one storm at a time
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	// /srcips lets box 1 learn the storm's source IPs so it can open its relay
	// port to all of them (the storm spreads across every IP here, not just B2).
	mux.HandleFunc("/srcips", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(srcIPs)
	})
	mux.HandleFunc("/run", func(w http.ResponseWriter, r *http.Request) {
		if !busy.TryLock() {
			http.Error(w, "busy: a storm is already running", http.StatusConflict)
			return
		}
		defer busy.Unlock()
		q := r.URL.Query()
		sips := srcIPs
		if v := qstr(q, "srcips", ""); v != "" {
			got, err := storm.ResolveSrcIPs(v)
			if err != nil {
				http.Error(w, "bad srcips: "+err.Error(), http.StatusBadRequest)
				return
			}
			sips = got
		}
		cfg := storm.Config{
			Relay:    qstr(q, "relay", *relay),
			ReqLen:   qint(q, "reqlen", *reqLen),
			ReplyLen: qint(q, "replylen", *replyLen),
			InFlight: qint(q, "inflight", 512),
			Warmup:   qdur(q, "warmup", 5*time.Second),
			Duration: qdur(q, "duration", 90*time.Second),
			SrcIPs:   sips,
		}
		if cfg.Relay == "" {
			http.Error(w, "no relay target: set -relay on the daemon or ?relay=ip:port", http.StatusBadRequest)
			return
		}
		log.Printf("/run relay=%s inflight=%d warmup=%v duration=%v srcips=%d", cfg.Relay, cfg.InFlight, cfg.Warmup, cfg.Duration, len(cfg.SrcIPs))
		res := storm.Run(cfg)
		log.Printf("/run done: completed=%d conn/s=%.0f p99=%.0fus auditFail=%d",
			res.Completed, res.ConnPerSec, res.P99us, res.AuditFail)
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	})

	log.Printf("loadgend control on %s (sink=%q default-relay=%q srcips=%d:%v)", *control, *sinkAddr, *relay, len(srcIPs), srcIPs)
	log.Fatal(http.ListenAndServe(*control, mux))
}

func qstr(q map[string][]string, k, def string) string {
	if v, ok := q[k]; ok && len(v) > 0 && v[0] != "" {
		return v[0]
	}
	return def
}

func qint(q map[string][]string, k string, def int) int {
	if v := qstr(q, k, ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func qdur(q map[string][]string, k string, def time.Duration) time.Duration {
	if v := qstr(q, k, ""); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
