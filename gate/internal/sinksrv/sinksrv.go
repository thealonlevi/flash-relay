//go:build linux

// Package sinksrv is the gate's upstream server, factored out so both the
// `sink` CLI and the `loadgend` control daemon can run it. Reads exactly ReqLen
// request bytes (auditing the request pattern), writes ReplyLen reply bytes,
// closes. Infrastructure — may use net. See gate/DESIGN.md §1–§2.
package sinksrv

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync/atomic"
	"time"

	"github.com/thealonlevi/flash-relay/gate/internal/proto"
)

func writeStat(path string, n uint64) {
	tmp := path + ".tmp"
	if os.WriteFile(tmp, []byte(fmt.Sprintf("served=%d\n", n)), 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

// countingWriter tallies bytes written through it (the echoed-byte counter for
// the throughput two-fd anti-cheat: it proves traffic actually reached the
// upstream sink, not a relay self-echo short-circuit).
type countingWriter struct {
	n *atomic.Uint64
	w io.Writer
}

func (cw countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n.Add(uint64(n))
	return n, err
}

// ListenAndServeEcho serves a long-lived ECHO upstream on addr: every accepted
// connection echoes whatever it receives until the peer closes. Used by B3
// (continuous bidirectional relay of long-lived tunnels) and the bytes_per_cpu
// throughput arm. If statsPath != "", atomically writes 'echoed=<bytes>' there
// every 250ms — the referee uses it to prove the relay forwarded to the sink
// rather than self-echoing (the two-fd anti-cheat for the throughput objective).
// Blocks.
func ListenAndServeEcho(addr, statsPath string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("echo sink listening on %s", addr)
	var live atomic.Int64
	var echoed atomic.Uint64
	go func() {
		for range time.Tick(5 * time.Second) {
			log.Printf("echo sink live=%d echoed=%d", live.Load(), echoed.Load())
		}
	}()
	if statsPath != "" {
		go func() {
			for range time.Tick(250 * time.Millisecond) {
				tmp := statsPath + ".tmp"
				if os.WriteFile(tmp, []byte(fmt.Sprintf("echoed=%d\n", echoed.Load())), 0o644) == nil {
					_ = os.Rename(tmp, statsPath)
				}
			}
		}()
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go func(c net.Conn) {
			live.Add(1)
			defer func() { live.Add(-1); c.Close() }()
			io.Copy(countingWriter{&echoed, c}, c) // echo until peer closes, tallying bytes
		}(c)
	}
}

// ListenAndServe serves the sink protocol on addr until the listener errors.
// Blocks. Logs counters every 2s. If statsPath != "", atomically writes
// 'served=<n>' there every 250ms (the optimizer referee uses this to prove the
// relay actually dialed upstream — the two-fd anti-cheat check).
func ListenAndServe(addr string, reqLen, replyLen int, statsPath string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("sink listening on %s (reqlen=%d replylen=%d)", addr, reqLen, replyLen)

	want := proto.Request(reqLen)
	reply := proto.Reply(replyLen)
	var served, auditFail, errs atomic.Uint64
	go func() {
		for range time.Tick(2 * time.Second) {
			log.Printf("sink served=%d auditFail=%d errs=%d", served.Load(), auditFail.Load(), errs.Load())
		}
	}()
	if statsPath != "" {
		go func() {
			for range time.Tick(250 * time.Millisecond) {
				writeStat(statsPath, served.Load())
			}
		}()
	}

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
				auditFail.Add(1)
			}
			if _, err := c.Write(reply); err != nil {
				errs.Add(1)
				return
			}
			served.Add(1)
		}(c)
	}
}
