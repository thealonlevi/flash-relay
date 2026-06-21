//go:build linux && amd64

// Command relay-uring is the gate SUT: the hand-rolled pure-Go io_uring relay.
// Each worker is a shared-nothing per-core engine: its own SO_REUSEPORT listener,
// io_uring ring, decision-hook pool, and connection map, pinned to one core. With
// -workers N you get N independent rings across N cores — NO shared scheduler,
// NO data-plane fd on the Go netpoller. That shared-nothing design is the answer
// to the netpoller's cross-core scheduler collapse under high concurrency.
//
// Per conn: single-shot accept (flood-safe) + recv/send relay; the decision hook
// runs OFF the ring on a goroutine pool (a slow dial parks one conn, not the ring);
// an eventfd read on the ring lets finished hook goroutines wake the worker; an
// always-armed timeout op makes io_uring_enter unable to wedge under a flood.
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

const (
	sysEventfd2         = 290 // linux/amd64
	sysSchedSetaffinity = 203 // linux/amd64
)

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
	opTimeout // periodic liveness timeout (deadlock-proofing under flood)
)

func ud(id uint64, op uint8) uint64   { return id<<8 | uint64(op) }
func unpack(u uint64) (uint64, uint8) { return u >> 8, uint8(u & 0xff) }

// gCompleted aggregates completed conns across all workers (for -statsfile).
var gCompleted atomic.Uint64

// pinToCore binds the calling OS thread to one CPU (sched_setaffinity), so each
// worker gets exactly one core — clean shared-nothing per-core engines.
func pinToCore(core int) {
	if core < 0 {
		return
	}
	var set [128]byte // cpu_set_t, 1024 CPUs
	set[core/8] |= 1 << (uint(core) % 8)
	syscall.Syscall(sysSchedSetaffinity, 0, uintptr(len(set)), uintptr(unsafe.Pointer(&set[0])))
}

// writeStat publishes the aggregate completed counter via an atomic file rename
// (plain file I/O — no netpoller). The 2-box harness reads this for conn/s.
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
	c2uBuf, u2cBuf     []byte
	c2uOff, c2uEnd     int  // pending send window c2uBuf[off:end] (client->upstream), for partial-send retry
	u2cOff, u2cEnd     int  // pending send window u2cBuf[off:end] (upstream->client)
	clientReadDone     bool // client half-closed its write (c->u EOF); upstream SHUT_WR propagated
	upstreamReadDone   bool // upstream half-closed (u->c EOF); client SHUT_WR propagated
	bytesC2U, bytesU2C uint64
}

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
	syscall.Write(b.efd, (*[8]byte)(unsafe.Pointer(&one))[:])
}

func (b *bridge) drain() []hookResult {
	b.mu.Lock()
	r := b.ready
	b.ready = nil
	b.mu.Unlock()
	return r
}

// relayCfg is the immutable, shared config every worker reads.
type relayCfg struct {
	addr, sinkIP      string
	port, sinkPort    int
	reqLen, replyLen  int
	authCPU           time.Duration
	delay             hook.DelayFunc
	ringSize          uint
	hookWorkers       int
	duplex            bool
	bufSize, maxConns int
	acceptBatch       int
}

func main() {
	addr := flag.String("addr", "0.0.0.0", "listen IP")
	port := flag.Int("port", 9000, "listen port")
	sinkIP := flag.String("sinkip", "127.0.0.1", "upstream sink IP")
	sinkPort := flag.Int("sinkport", 9100, "upstream sink port")
	reqLen := flag.Int("reqlen", proto.DefaultReqLen, "initial request bytes to read")
	replyLen := flag.Int("replylen", proto.DefaultReplyLen, "upstream reply bytes buffer")
	authCPU := flag.Duration("authcpu", 5*time.Microsecond, "auth CPU busy-spin per conn")
	ringSize := flag.Uint("ring", 4096, "io_uring SQ entries per worker")
	hookWorkers := flag.Int("hookworkers", 256, "off-ring decision-hook goroutines per worker")
	realistic := flag.Bool("realistic", false, "realistic-dial: sample ms-scale dial latency")
	dialP50 := flag.Float64("dialp50", 20, "realistic dial median ms")
	dialSigma := flag.Float64("dialsigma", 0.9, "realistic dial log-space sigma")
	dialCap := flag.Float64("dialcap", 30000, "realistic dial cap ms (dial timeout)")
	statsFile := flag.String("statsfile", "", "if set, write aggregate 'completed=<n>' here every 250ms")
	duplex := flag.Bool("duplex", false, "continuous bidirectional relay (long-lived tunnels, B3)")
	bufSize := flag.Int("bufsize", 16384, "per-direction relay buffer bytes (duplex mode)")
	maxConns := flag.Int("maxconns", 50000, "accept backpressure cap PER WORKER: shed above this many live")
	acceptBatch := flag.Int("acceptbatch", 64, "accepts kept in flight per worker (bounded parallelism: throughput without flooding the CQ)")
	workers := flag.Int("workers", 1, "number of shared-nothing per-core ring workers (SO_REUSEPORT)")
	startCore := flag.Int("startcore", -1, "pin worker i to core startcore+i (-1 = no pinning)")
	flag.Parse()

	delay := hook.NoDelay()
	if *realistic {
		delay = hook.Lognormal(*dialP50, *dialSigma, *dialCap, 1)
	}
	c := &relayCfg{
		addr: *addr, sinkIP: *sinkIP, port: *port, sinkPort: *sinkPort,
		reqLen: *reqLen, replyLen: *replyLen, authCPU: *authCPU, delay: delay,
		ringSize: *ringSize, hookWorkers: *hookWorkers, duplex: *duplex,
		bufSize: *bufSize, maxConns: *maxConns, acceptBatch: *acceptBatch,
	}

	if *statsFile != "" {
		go func() {
			for range time.Tick(250 * time.Millisecond) {
				writeStat(*statsFile, gCompleted.Load())
			}
		}()
	}

	log.Printf("relay-uring (SUT) :%d -> sink %s:%d  workers=%d startcore=%d duplex=%v maxconns=%d/worker",
		*port, *sinkIP, *sinkPort, *workers, *startCore, *duplex, *maxConns)

	// Create the N SO_REUSEPORT listeners SEQUENTIALLY here (the kernel's
	// reuseport-group setup races if N sockets bind the same port concurrently),
	// then hand one to each worker. Kernel load-balances accepts across them.
	for i := 0; i < *workers; i++ {
		ln, err := rawsock.Listen(c.addr, c.port, 4096)
		if err != nil {
			log.Fatalf("listener %d: %v", i, err)
		}
		core := -1
		if *startCore >= 0 {
			core = *startCore + i
		}
		go worker(i, core, ln, c)
	}
	select {} // workers run forever
}

// worker is one shared-nothing per-core ring engine.
func worker(id, core int, ln *rawsock.Listener, c *relayCfg) {
	runtime.LockOSThread()
	pinToCore(core)

	efd, _, errno := syscall.Syscall(sysEventfd2, 0, 0, 0)
	if errno != 0 {
		log.Fatalf("worker %d eventfd2: %v", id, errno)
	}
	br := &bridge{efd: int(efd)}
	hcfg := hook.Config{AuthCPU: c.authCPU, Delay: c.delay, SinkIP: c.sinkIP, SinkPort: c.sinkPort}
	jobs := make(chan uint64, 1<<16)
	for i := 0; i < c.hookWorkers; i++ {
		go func() {
			for cid := range jobs {
				fd, err := hcfg.Decide()
				br.push(hookResult{id: cid, upstreamFD: fd, ok: err == nil})
			}
		}()
	}

	ring, err := uring.New(uint32(c.ringSize))
	if err != nil {
		log.Fatalf("worker %d uring.New: %v", id, err)
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
			ring.Submit(0)
		}
	}
	closeConn := func(cc *conn) {
		if cc.closing {
			return
		}
		cc.closing = true
		cc.closesLeft = 0
		if cc.clientFD > 0 {
			cc.closesLeft++
			post(func(s *uring.SQE) { uring.PrepClose(s, cc.clientFD, ud(cc.id, opClose)) })
		}
		if cc.upstreamFD > 0 {
			cc.closesLeft++
			post(func(s *uring.SQE) { uring.PrepClose(s, cc.upstreamFD, ud(cc.id, opClose)) })
		}
		if cc.closesLeft == 0 {
			delete(conns, cc.id)
		}
	}

	acceptInflight := 0 // bounded-batch accept: keep up to c.acceptBatch in flight
	postAccept := func() {
		post(func(s *uring.SQE) { uring.PrepAccept(s, ln.FD, ud(0, opAccept)) })
		acceptInflight++
	}
	postEventfd := func() {
		post(func(s *uring.SQE) { uring.PrepRead(s, br.efd, efdBuf, ud(0, opEventfd)) })
	}
	tspec := uring.Timespec{Sec: 0, Nsec: 100 * 1000 * 1000}
	postTimeout := func() {
		post(func(s *uring.SQE) { uring.PrepTimeout(s, &tspec, ud(0, opTimeout)) })
	}

	postAccept()
	postEventfd()
	postTimeout()

	var accepted, completed, shed, errs uint64
	lastLog := time.Now()

	for {
		if _, err := ring.Submit(1); err != nil {
			log.Fatalf("worker %d submit: %v", id, err)
		}
		n := ring.CQReady()
		for i := uint32(0); i < n; i++ {
			cqe := ring.PeekCQE(i)
			cid, op := unpack(cqe.UserData)
			res := cqe.Res
			switch op {
			case opAccept:
				acceptInflight-- // this accept SQE consumed; main loop tops the batch back up
				if res < 0 {
					errs++
					break
				}
				accepted++
				if len(conns) >= c.maxConns {
					shed++
					post(func(s *uring.SQE) { uring.PrepClose(s, int(res), ud(0, opClose)) })
					break
				}
				nextID++
				cc := &conn{id: nextID, clientFD: int(res), reqBuf: make([]byte, c.reqLen), respBuf: make([]byte, c.replyLen)}
				conns[cc.id] = cc
				ncid := cc.id
				post(func(s *uring.SQE) { uring.PrepRecv(s, cc.clientFD, cc.reqBuf, ud(ncid, opRecvReq)) })

			case opEventfd:
				postEventfd()
				for _, r := range br.drain() {
					cc := conns[r.id]
					if cc == nil {
						if r.ok && r.upstreamFD > 0 {
							syscall.Close(r.upstreamFD)
						}
						continue
					}
					if !r.ok {
						errs++
						closeConn(cc)
						continue
					}
					cc.upstreamFD = r.upstreamFD
					post(func(s *uring.SQE) { uring.PrepSend(s, cc.upstreamFD, cc.reqBuf[:cc.reqN], 0, ud(cc.id, opSendUp)) })
				}

			case opRecvReq:
				cc := conns[cid]
				if cc == nil {
					break
				}
				if res <= 0 {
					errs++
					closeConn(cc)
					break
				}
				cc.reqN = int(res)
				jobs <- cc.id

			case opSendUp:
				cc := conns[cid]
				if cc == nil {
					break
				}
				if res < 0 {
					errs++
					closeConn(cc)
					break
				}
				if c.duplex {
					cc.c2uBuf = make([]byte, c.bufSize)
					cc.u2cBuf = make([]byte, c.bufSize)
					post(func(s *uring.SQE) { uring.PrepRecv(s, cc.clientFD, cc.c2uBuf, ud(cc.id, opC2URecv)) })
					post(func(s *uring.SQE) { uring.PrepRecv(s, cc.upstreamFD, cc.u2cBuf, ud(cc.id, opU2CRecv)) })
					break
				}
				post(func(s *uring.SQE) { uring.PrepRecv(s, cc.upstreamFD, cc.respBuf, ud(cc.id, opRecvResp)) })

			case opC2URecv: // client -> upstream: data from client
				cc := conns[cid]
				if cc == nil || cc.closing {
					break
				}
				if res < 0 { // error
					closeConn(cc)
					break
				}
				if res == 0 { // client half-closed its write: propagate EOF to upstream
					syscall.Shutdown(cc.upstreamFD, syscall.SHUT_WR)
					cc.clientReadDone = true
					if cc.upstreamReadDone {
						closeConn(cc) // both directions drained
					}
					break
				}
				cc.bytesC2U += uint64(res)
				cc.c2uOff, cc.c2uEnd = 0, int(res)
				post(func(s *uring.SQE) {
					uring.PrepSend(s, cc.upstreamFD, cc.c2uBuf[cc.c2uOff:cc.c2uEnd], 0, ud(cc.id, opC2USend))
				})

			case opC2USend:
				cc := conns[cid]
				if cc == nil || cc.closing {
					break
				}
				if res <= 0 { // send error / 0 -> peer gone
					closeConn(cc)
					break
				}
				cc.c2uOff += int(res)
				if cc.c2uOff < cc.c2uEnd { // PARTIAL send: forward the remainder
					post(func(s *uring.SQE) {
						uring.PrepSend(s, cc.upstreamFD, cc.c2uBuf[cc.c2uOff:cc.c2uEnd], 0, ud(cc.id, opC2USend))
					})
					break
				}
				if !cc.clientReadDone { // fully forwarded -> read more from client
					post(func(s *uring.SQE) { uring.PrepRecv(s, cc.clientFD, cc.c2uBuf, ud(cc.id, opC2URecv)) })
				}

			case opU2CRecv: // upstream -> client: data from upstream
				cc := conns[cid]
				if cc == nil || cc.closing {
					break
				}
				if res < 0 {
					closeConn(cc)
					break
				}
				if res == 0 { // upstream half-closed: propagate EOF to client
					syscall.Shutdown(cc.clientFD, syscall.SHUT_WR)
					cc.upstreamReadDone = true
					if cc.clientReadDone {
						closeConn(cc)
					}
					break
				}
				cc.bytesU2C += uint64(res)
				cc.u2cOff, cc.u2cEnd = 0, int(res)
				post(func(s *uring.SQE) {
					uring.PrepSend(s, cc.clientFD, cc.u2cBuf[cc.u2cOff:cc.u2cEnd], 0, ud(cc.id, opU2CSend))
				})

			case opU2CSend:
				cc := conns[cid]
				if cc == nil || cc.closing {
					break
				}
				if res <= 0 {
					closeConn(cc)
					break
				}
				cc.u2cOff += int(res)
				if cc.u2cOff < cc.u2cEnd { // PARTIAL send: forward the remainder
					post(func(s *uring.SQE) {
						uring.PrepSend(s, cc.clientFD, cc.u2cBuf[cc.u2cOff:cc.u2cEnd], 0, ud(cc.id, opU2CSend))
					})
					break
				}
				if !cc.upstreamReadDone {
					post(func(s *uring.SQE) { uring.PrepRecv(s, cc.upstreamFD, cc.u2cBuf, ud(cc.id, opU2CRecv)) })
				}

			case opRecvResp:
				cc := conns[cid]
				if cc == nil {
					break
				}
				if res <= 0 {
					errs++
					closeConn(cc)
					break
				}
				rn := int(res)
				post(func(s *uring.SQE) { uring.PrepSend(s, cc.clientFD, cc.respBuf[:rn], 0, ud(cc.id, opSendClient)) })

			case opSendClient:
				cc := conns[cid]
				if cc == nil {
					break
				}
				if res < 0 {
					errs++
				} else {
					completed++
					gCompleted.Add(1)
				}
				closeConn(cc)

			case opClose:
				cc := conns[cid]
				if cc == nil {
					break
				}
				cc.closesLeft--
				if cc.closesLeft <= 0 {
					delete(conns, cid)
				}

			case opTimeout:
				postTimeout()
			}
		}
		ring.CQAdvance(n)

		// Top the accept batch back up: keep up to acceptBatch accepts in flight,
		// bounded so live+inflight never exceeds the cap (backpressure) and the
		// batch stays tiny vs the CQ (flood-safe — no CQ overflow).
		for acceptInflight < c.acceptBatch && len(conns)+acceptInflight < c.maxConns {
			postAccept()
		}

		if id == 0 && time.Since(lastLog) >= 2*time.Second {
			log.Printf("uring w0 accepted=%d completed=%d shed=%d errs=%d live=%d (agg completed=%d)",
				accepted, completed, shed, errs, len(conns), gCompleted.Load())
			lastLog = time.Now()
		}
	}
}
