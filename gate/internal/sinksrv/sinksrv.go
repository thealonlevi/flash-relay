//go:build linux

// Package sinksrv is the gate's upstream server, factored out so both the
// `sink` CLI and the `loadgend` control daemon can run it. Reads exactly ReqLen
// request bytes (auditing the request pattern), writes ReplyLen reply bytes,
// closes. Infrastructure — may use net. See gate/DESIGN.md §1–§2.
package sinksrv

import (
	"io"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/thealonlevi/flash-relay/gate/internal/proto"
)

// ListenAndServe serves the sink protocol on addr until the listener errors.
// Blocks. Logs counters every 2s.
func ListenAndServe(addr string, reqLen, replyLen int) error {
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
