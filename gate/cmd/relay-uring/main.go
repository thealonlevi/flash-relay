//go:build linux && amd64

// Command relay-uring is the gate SUT: the hand-rolled pure-Go io_uring relay
// probe. One core, two fds per conn, single-shot accept/recv/send, recv/send
// relay (splice is B4). The decision hook runs OFF the ring on a worker-goroutine
// pool; a slow dial parks one connection while the ring keeps going. An eventfd
// read posted on the ring lets finished hook goroutines wake the worker.
//
// NO data-plane fd ever touches the Go netpoller — this is the whole point (B1).
// See gate/DESIGN.md and RELAY_PLAN.md.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/thealonlevi/flash-relay/gate/internal/hook"
	"github.com/thealonlevi/flash-relay/gate/internal/proto"
	"github.com/thealonlevi/flash-relay/gate/internal/rawsock"
	"github.com/thealonlevi/flash-relay/gate/internal/uring"
)

const sysEventfd2 = 290 // linux/amd64

// op types, packed into the low 8 bits of user_data; conn id in the high bits.
const (
	opAccept = iota + 1
	opEventfd
	opRecvReq
	opSendUp
	opRecvResp
	opSendClient
	opClose
	// duplex (continuous bidirectional relay) ops:
	opC2URecv // recv on client
	opC2USend // send to upstream
	opU2CRecv // recv on upstream
	opU2CSend // send to client
)

func ud(id uint64, op uint8) uint64 { return id<<8 | uint64(op) }
func unpack(u uint64) (uint64, uint8) { return u >> 8, uint8(u & 0xff) }

// writeStat publishes the completed counter via an atomic file rename (plain
// file I/O — no netpoller). The 2-box harness reads this to compute conn/s on
// the SUT box itself.
func writeStat(path string, n uint64) {
	tmp := path + ".tmp"
	if os.WriteFile(tmp, []byte(fmt.Sprintf("completed=%d\n", n)), 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

type conn struct {
	id         uint64
	clientFD   int
	upstreamFD int
	reqBuf     []byte
	respBuf    []byte
	reqN       int
	closing    bool
	closesLeft int
	// duplex relay state (allocated only in -duplex mode)
	c2uBuf   []byte // client -> upstream
	u2cBuf   []byte // upstream -> client
	bytesC2U uint64
	bytesU2C uint64
}

// hookResult is produced by an off-ring hook goroutine and consumed by the ring
// worker after an eventfd wakeup.
type hookResult struct {
	id         uint64
	upstreamFD int
	ok         bool
}

type bridge struct {
	mu    sync.Mutex
	ready []hookResult
	efd   int
}

func (b *bridge) push(r hookResult) {
	b.mu.Lock()
	b.ready = append(b.ready, r)
	b.mu.Unlock()
	var one uint64 = 1
	syscall.Write(b.efd, (*[8]byte)(unsafe.Pointer(&one))[:]) // wake the ring worker
}

func (b *bridge) drain() []hookResult {
	b.mu.Lock()
	r := b.ready
	b.ready = nil
	b.mu.Unlock()
	return r
}

func main() {
	addr := flag.String("addr", "0.0.0.0", "listen IP")
	port := flag.Int("port", 9000, "listen port")
	sinkIP := flag.String("sinkip", "127.0.0.1", "upstream sink IP")
	sinkPort := flag.Int("sinkport", 9100, "upstream sink port")
	reqLen := flag.Int("reqlen", proto.DefaultReqLen, "initial request bytes to read")
	replyLen := flag.Int("replylen", proto.DefaultReplyLen, "upstream reply bytes buffer")
	authCPU := flag.Duration("authcpu", 5*time.Microsecond, "auth CPU busy-spin per conn")
	ringSize := flag.Uint("ring", 4096, "io_uring SQ entries")
	hookWorkers := flag.Int("hookworkers", 256, "off-ring decision-hook goroutines")
	realistic := flag.Bool("realistic", false, "realistic-dial: sample ms-scale dial latency")
	dialP50 := flag.Float64("dialp50", 20, "realistic dial median ms")
	dialSigma := flag.Float64("dialsigma", 0.9, "realistic dial log-space sigma")
	dialCap := flag.Float64("dialcap", 30000, "realistic dial cap ms (dial timeout)")
	statsFile := flag.String("statsfile", "", "if set, atomically write 'completed=<n>' here every 250ms (2-box harness; no netpoller)")
	duplex := flag.Bool("duplex", false, "continuous bidirectional relay (long-lived tunnels, B3) instead of one-shot churn")
	bufSize := flag.Int("bufsize", 16384, "per-direction relay buffer bytes (duplex mode)")
	flag.Parse()

	var delay hook.DelayFunc = hook.NoDelay()
	if *realistic {
		delay = hook.Lognormal(*dialP50, *dialSigma, *dialCap, 1)
	}
	hcfg := hook.Config{AuthCPU: *authCPU, Delay: delay, SinkIP: *sinkIP, SinkPort: *sinkPort}

	ln, err := rawsock.Listen(*addr, *port, 4096)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("relay-uring (SUT) on %s:%d -> sink %s:%d (authcpu=%v realistic=%v hookworkers=%d)",
		*addr, ln.Port, *sinkIP, *sinkPort, *authCPU, *realistic, *hookWorkers)

	efd, _, errno := syscall.Syscall(sysEventfd2, 0, 0, 0)
	if errno != 0 {
		log.Fatalf("eventfd2: %v", errno)
	}
	br := &bridge{efd: int(efd)}

	// Off-ring decision-hook pool. Jobs are conn ids; results go via the bridge.
	jobs := make(chan uint64, 1<<16)
	for i := 0; i < *hookWorkers; i++ {
		go func() {
			for id := range jobs {
				fd, err := hcfg.Decide()
				br.push(hookResult{id: id, upstreamFD: fd, ok: err == nil})
			}
		}()
	}

	runtime.LockOSThread() // pin the ring worker to its OS thread
	ring, err := uring.New(uint32(*ringSize))
	if err != nil {
		log.Fatalf("uring.New: %v", err)
	}
	defer ring.Close()

	conns := make(map[uint64]*conn, 1<<16)
	var nextID uint64
	efdBuf := make([]byte, 8)

	post := func(prep func(*uring.SQE)) {
		for {
			if s := ring.GetSQE(); s != nil {
				prep(s)
				return
			}
			ring.Submit(0) // SQ full: flush to free entries, then retry
		}
	}

	closeConn := func(c *conn) {
		if c.closing {
			return
		}
		c.closing = true
		c.closesLeft = 0
		if c.clientFD > 0 {
			c.closesLeft++
			post(func(s *uring.SQE) { uring.PrepClose(s, c.clientFD, ud(c.id, opClose)) })
		}
		if c.upstreamFD > 0 {
			c.closesLeft++
			post(func(s *uring.SQE) { uring.PrepClose(s, c.upstreamFD, ud(c.id, opClose)) })
		}
		if c.closesLeft == 0 {
			delete(conns, c.id)
		}
	}

	postAccept := func() {
		post(func(s *uring.SQE) { uring.PrepAcceptMultishot(s, ln.FD, ud(0, opAccept)) })
	}
	postEventfd := func() {
		post(func(s *uring.SQE) { uring.PrepRead(s, br.efd, efdBuf, ud(0, opEventfd)) })
	}

	postAccept()
	postEventfd()

	var accepted, completed, errs uint64
	var completedStat atomic.Uint64
	if *statsFile != "" {
		go func() {
			for range time.Tick(250 * time.Millisecond) {
				writeStat(*statsFile, completedStat.Load())
			}
		}()
	}
	lastLog := time.Now()

	for {
		if _, err := ring.Submit(1); err != nil {
			log.Fatalf("submit: %v", err)
		}
		n := ring.CQReady()
		for i := uint32(0); i < n; i++ {
			cqe := ring.PeekCQE(i)
			id, op := unpack(cqe.UserData)
			res := cqe.Res
			switch op {
			case opAccept:
				// Multishot accept stays armed across connections; only re-arm
				// when the kernel signals the SQE is done (CQEFMore cleared).
				if cqe.Flags&uring.CQEFMore == 0 {
					postAccept()
				}
				if res < 0 {
					errs++
					break
				}
				accepted++
				nextID++
				c := &conn{
					id:       nextID,
					clientFD: int(res),
					reqBuf:   make([]byte, *reqLen),
					respBuf:  make([]byte, *replyLen),
				}
				conns[c.id] = c
				cid := c.id
				post(func(s *uring.SQE) { uring.PrepRecv(s, c.clientFD, c.reqBuf, ud(cid, opRecvReq)) })

			case opEventfd:
				postEventfd()
				for _, r := range br.drain() {
					c := conns[r.id]
					if c == nil {
						if r.ok && r.upstreamFD > 0 {
							syscall.Close(r.upstreamFD)
						}
						continue
					}
					if !r.ok {
						errs++
						closeConn(c)
						continue
					}
					c.upstreamFD = r.upstreamFD
					post(func(s *uring.SQE) {
						uring.PrepSend(s, c.upstreamFD, c.reqBuf[:c.reqN], 0, ud(c.id, opSendUp))
					})
				}

			case opRecvReq:
				c := conns[id]
				if c == nil {
					break
				}
				if res <= 0 {
					errs++
					closeConn(c)
					break
				}
				c.reqN = int(res)
				jobs <- c.id // hand off to off-ring decision hook

			case opSendUp:
				c := conns[id]
				if c == nil {
					break
				}
				if res < 0 {
					errs++
					closeConn(c)
					break
				}
				if *duplex {
					// Initial request forwarded; go full duplex. One recv
					// outstanding per direction; re-armed after its send.
					c.c2uBuf = make([]byte, *bufSize)
					c.u2cBuf = make([]byte, *bufSize)
					post(func(s *uring.SQE) { uring.PrepRecv(s, c.clientFD, c.c2uBuf, ud(c.id, opC2URecv)) })
					post(func(s *uring.SQE) { uring.PrepRecv(s, c.upstreamFD, c.u2cBuf, ud(c.id, opU2CRecv)) })
					break
				}
				post(func(s *uring.SQE) {
					uring.PrepRecv(s, c.upstreamFD, c.respBuf, ud(c.id, opRecvResp))
				})

			case opC2URecv: // client -> upstream
				c := conns[id]
				if c == nil || c.closing {
					break
				}
				if res <= 0 { // client half-closed or error
					closeConn(c)
					break
				}
				n := int(res)
				c.bytesC2U += uint64(n)
				post(func(s *uring.SQE) { uring.PrepSend(s, c.upstreamFD, c.c2uBuf[:n], 0, ud(c.id, opC2USend)) })

			case opC2USend:
				c := conns[id]
				if c == nil || c.closing {
					break
				}
				if res < 0 {
					closeConn(c)
					break
				}
				post(func(s *uring.SQE) { uring.PrepRecv(s, c.clientFD, c.c2uBuf, ud(c.id, opC2URecv)) })

			case opU2CRecv: // upstream -> client
				c := conns[id]
				if c == nil || c.closing {
					break
				}
				if res <= 0 { // upstream half-closed or error
					closeConn(c)
					break
				}
				n := int(res)
				c.bytesU2C += uint64(n)
				post(func(s *uring.SQE) { uring.PrepSend(s, c.clientFD, c.u2cBuf[:n], 0, ud(c.id, opU2CSend)) })

			case opU2CSend:
				c := conns[id]
				if c == nil || c.closing {
					break
				}
				if res < 0 {
					closeConn(c)
					break
				}
				post(func(s *uring.SQE) { uring.PrepRecv(s, c.upstreamFD, c.u2cBuf, ud(c.id, opU2CRecv)) })

			case opRecvResp:
				c := conns[id]
				if c == nil {
					break
				}
				if res <= 0 {
					errs++
					closeConn(c)
					break
				}
				rn := int(res)
				post(func(s *uring.SQE) {
					uring.PrepSend(s, c.clientFD, c.respBuf[:rn], 0, ud(c.id, opSendClient))
				})

			case opSendClient:
				c := conns[id]
				if c == nil {
					break
				}
				if res < 0 {
					errs++
				} else {
					completed++
					completedStat.Store(completed)
				}
				closeConn(c)

			case opClose:
				c := conns[id]
				if c == nil {
					break
				}
				c.closesLeft--
				if c.closesLeft <= 0 {
					delete(conns, id)
				}
			}
		}
		ring.CQAdvance(n)

		if time.Since(lastLog) >= 2*time.Second {
			log.Printf("uring accepted=%d completed=%d errs=%d live=%d",
				accepted, completed, errs, len(conns))
			lastLog = time.Now()
		}
	}
}
