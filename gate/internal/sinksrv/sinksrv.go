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

// ListenAndServeEcho serves a long-lived ECHO upstream on addr: every accepted
// connection echoes whatever it receives until the peer closes. Used by B3
// (continuous bidirectional relay of long-lived tunnels). Blocks.
func ListenAndServeEcho(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("echo sink listening on %s", addr)
	var live atomic.Int64
	go func() {
		for range time.Tick(5 * time.Second) {
			log.Printf("echo sink live=%d", live.Load())
		}
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			continue
		}
		go func(c net.Conn) {
			live.Add(1)
			defer func() { live.Add(-1); c.Close() }()
			io.Copy(c, c) // echo until peer closes
		}(c)
	}
}

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
