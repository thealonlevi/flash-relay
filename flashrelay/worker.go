//go:build linux && amd64

package flashrelay

import (
	"log"
	"net/netip"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"github.com/thealonlevi/flash-relay/internal/rawsock"
	"github.com/thealonlevi/flash-relay/internal/uring"
)

const (
	sysEventfd2         = 290 // linux/amd64
	sysSchedSetaffinity = 203 // linux/amd64
)

// op types, packed into the low 8 bits of user_data; conn id in the high bits.
const (
	opAccept = iota + 1
	opEventfd
	opRecvReq     // read the initial request from the client (pre-hook)
	opReplyClient // relay path: send Decision.Reply to client before forwarding
	opSendUp      // forward the initial request to the upstream
	opRejectSend  // reject path: send Decision.Reply to client, then close
	opC2URecv
	opC2USend
	opU2CRecv
	opU2CSend
	opClose
	opTimeout
)

func ud(id uint64, op uint8) uint64   { return id<<8 | uint64(op) }
func unpack(u uint64) (uint64, uint8) { return u >> 8, uint8(u & 0xff) }

func pinToCore(core int) {
	if core < 0 {
		return
	}
	var set [128]byte
	set[core/8] |= 1 << (uint(core) % 8)
	syscall.Syscall(sysSchedSetaffinity, 0, uintptr(len(set)), uintptr(unsafe.Pointer(&set[0])))
}

func peerOf(fd int) netip.AddrPort {
	sa, err := syscall.Getpeername(fd)
	if err != nil {
		return netip.AddrPort{}
	}
	switch a := sa.(type) {
	case *syscall.SockaddrInet4:
		return netip.AddrPortFrom(netip.AddrFrom4(a.Addr), uint16(a.Port))
	case *syscall.SockaddrInet6:
		return netip.AddrPortFrom(netip.AddrFrom16(a.Addr), uint16(a.Port))
	}
	return netip.AddrPort{}
}

type conn struct {
	id                 uint64
	clientFD           int
	upstreamFD         int
	reqBuf             []byte
	reqN               int
	initSent           int // initial-request -> upstream send progress
	replyBuf           []byte
	replyOff, replyEnd int // optional reply -> client send progress
	closing            bool
	closesLeft         int
	lastTick           uint64
	c2uBuf, u2cBuf     []byte
	c2uOff, c2uEnd     int
	u2cOff, u2cEnd     int
	clientReadDone     bool
	upstreamReadDone   bool
}

type job struct {
	id       uint64
	clientFD int
	req      []byte
}

type hookResult struct {
	id  uint64
	dec Decision
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

// runWorker is one shared-nothing per-core ring engine.
func (s *Server) runWorker(id, core int, ln *rawsock.Listener) {
	defer s.wg.Done()
	runtime.LockOSThread()
	pinToCore(core)
	cfg := &s.cfg

	efd, _, errno := syscall.Syscall(sysEventfd2, 0, 0, 0)
	if errno != 0 {
		log.Printf("flashrelay worker %d: eventfd2: %v", id, errno)
		return
	}
	br := &bridge{efd: int(efd)}
	jobs := make(chan job, 1<<16)
	for i := 0; i < cfg.HookWorkers; i++ {
		go func() {
			for j := range jobs {
				dec := s.hook(j.req, peerOf(j.clientFD))
				br.push(hookResult{id: j.id, dec: dec})
			}
		}()
	}

	ring, err := uring.New(uint32(cfg.RingSize))
	if err != nil {
		log.Printf("flashrelay worker %d: uring.New: %v", id, err)
		return
	}
	defer ring.Close()

	conns := make(map[uint64]*conn, 1<<16)
	var nextID, tick uint64
	efdBuf := make([]byte, 8)
	idleTicks := uint64(0)
	if cfg.IdleTimeout > 0 {
		idleTicks = uint64(cfg.IdleTimeout / (100 * 1e6)) // timeout op fires every 100ms
		if idleTicks == 0 {
			idleTicks = 1
		}
	}

	post := func(prep func(*uring.SQE)) {
		for {
			if sq := ring.GetSQE(); sq != nil {
				prep(sq)
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
		s.cnt.live.Add(^uint64(0)) // -1
		cc.closesLeft = 0
		if cc.clientFD > 0 {
			cc.closesLeft++
			post(func(sq *uring.SQE) { uring.PrepClose(sq, cc.clientFD, ud(cc.id, opClose)) })
		}
		if cc.upstreamFD > 0 {
			cc.closesLeft++
			post(func(sq *uring.SQE) { uring.PrepClose(sq, cc.upstreamFD, ud(cc.id, opClose)) })
		}
		if cc.closesLeft == 0 {
			delete(conns, cc.id)
		}
	}
	startDuplex := func(cc *conn) {
		cc.c2uBuf = make([]byte, cfg.BufSize)
		cc.u2cBuf = make([]byte, cfg.BufSize)
		post(func(sq *uring.SQE) { uring.PrepRecv(sq, cc.clientFD, cc.c2uBuf, ud(cc.id, opC2URecv)) })
		post(func(sq *uring.SQE) { uring.PrepRecv(sq, cc.upstreamFD, cc.u2cBuf, ud(cc.id, opU2CRecv)) })
	}

	acceptInflight := 0
	postAccept := func() {
		post(func(sq *uring.SQE) { uring.PrepAccept(sq, ln.FD, ud(0, opAccept)) })
		acceptInflight++
	}
	postEventfd := func() { post(func(sq *uring.SQE) { uring.PrepRead(sq, br.efd, efdBuf, ud(0, opEventfd)) }) }
	tspec := uring.Timespec{Sec: 0, Nsec: 100 * 1000 * 1000}
	postTimeout := func() { post(func(sq *uring.SQE) { uring.PrepTimeout(sq, &tspec, ud(0, opTimeout)) }) }

	postEventfd()
	postTimeout()

	for {
		if _, err := ring.Submit(1); err != nil {
			log.Printf("flashrelay worker %d: submit: %v", id, err)
			return
		}
		n := ring.CQReady()
		for i := uint32(0); i < n; i++ {
			cqe := ring.PeekCQE(i)
			cid, op := unpack(cqe.UserData)
			res := cqe.Res
			switch op {
			case opAccept:
				acceptInflight--
				if res < 0 {
					s.cnt.errors.Add(1)
					break
				}
				s.cnt.accepted.Add(1)
				if len(conns) >= cfg.MaxConns {
					s.cnt.shed.Add(1)
					post(func(sq *uring.SQE) { uring.PrepClose(sq, int(res), ud(0, opClose)) })
					break
				}
				nextID++
				cc := &conn{id: nextID, clientFD: int(res), reqBuf: make([]byte, cfg.InitialReqLen), lastTick: tick}
				conns[cc.id] = cc
				s.cnt.live.Add(1)
				ncid := cc.id
				post(func(sq *uring.SQE) { uring.PrepRecv(sq, cc.clientFD, cc.reqBuf, ud(ncid, opRecvReq)) })

			case opRecvReq:
				cc := conns[cid]
				if cc == nil {
					break
				}
				if res <= 0 {
					s.cnt.errors.Add(1)
					closeConn(cc)
					break
				}
				cc.reqN = int(res)
				cc.lastTick = tick
				jobs <- job{id: cc.id, clientFD: cc.clientFD, req: cc.reqBuf[:cc.reqN]}

			case opEventfd:
				postEventfd()
				for _, r := range br.drain() {
					cc := conns[r.id]
					if cc == nil { // conn gone (shed/closed) — clean up the adopted fd
						if !r.dec.Reject && r.dec.UpstreamFD > 0 {
							syscall.Close(r.dec.UpstreamFD)
						}
						continue
					}
					cc.lastTick = tick
					if r.dec.Reject {
						s.cnt.rejected.Add(1)
						if len(r.dec.Reply) > 0 {
							cc.replyBuf, cc.replyOff, cc.replyEnd = r.dec.Reply, 0, len(r.dec.Reply)
							post(func(sq *uring.SQE) { uring.PrepSend(sq, cc.clientFD, cc.replyBuf, 0, ud(cc.id, opRejectSend)) })
						} else {
							closeConn(cc)
						}
						continue
					}
					cc.upstreamFD = r.dec.UpstreamFD
					if len(r.dec.Reply) > 0 { // send reply to client first, then forward request
						cc.replyBuf, cc.replyOff, cc.replyEnd = r.dec.Reply, 0, len(r.dec.Reply)
						post(func(sq *uring.SQE) { uring.PrepSend(sq, cc.clientFD, cc.replyBuf, 0, ud(cc.id, opReplyClient)) })
					} else {
						cc.initSent = 0
						post(func(sq *uring.SQE) { uring.PrepSend(sq, cc.upstreamFD, cc.reqBuf[:cc.reqN], 0, ud(cc.id, opSendUp)) })
					}
				}

			case opReplyClient: // relay path: reply-to-client done -> forward request to upstream
				cc := conns[cid]
				if cc == nil || cc.closing {
					break
				}
				if res <= 0 {
					closeConn(cc)
					break
				}
				cc.replyOff += int(res)
				if cc.replyOff < cc.replyEnd {
					post(func(sq *uring.SQE) {
						uring.PrepSend(sq, cc.clientFD, cc.replyBuf[cc.replyOff:cc.replyEnd], 0, ud(cc.id, opReplyClient))
					})
					break
				}
				cc.initSent = 0
				post(func(sq *uring.SQE) { uring.PrepSend(sq, cc.upstreamFD, cc.reqBuf[:cc.reqN], 0, ud(cc.id, opSendUp)) })

			case opRejectSend: // reject path: reply sent -> close
				cc := conns[cid]
				if cc == nil || cc.closing {
					break
				}
				if res <= 0 {
					closeConn(cc)
					break
				}
				cc.replyOff += int(res)
				if cc.replyOff < cc.replyEnd {
					post(func(sq *uring.SQE) {
						uring.PrepSend(sq, cc.clientFD, cc.replyBuf[cc.replyOff:cc.replyEnd], 0, ud(cc.id, opRejectSend))
					})
					break
				}
				closeConn(cc)

			case opSendUp: // initial request forwarded to upstream -> go duplex
				cc := conns[cid]
				if cc == nil || cc.closing {
					break
				}
				if res <= 0 {
					s.cnt.errors.Add(1)
					closeConn(cc)
					break
				}
				cc.initSent += int(res)
				if cc.initSent < cc.reqN {
					post(func(sq *uring.SQE) {
						uring.PrepSend(sq, cc.upstreamFD, cc.reqBuf[cc.initSent:cc.reqN], 0, ud(cc.id, opSendUp))
					})
					break
				}
				startDuplex(cc)

			case opC2URecv:
				cc := conns[cid]
				if cc == nil || cc.closing {
					break
				}
				if res < 0 {
					closeConn(cc)
					break
				}
				if res == 0 {
					syscall.Shutdown(cc.upstreamFD, syscall.SHUT_WR)
					cc.clientReadDone = true
					if cc.upstreamReadDone {
						closeConn(cc)
					}
					break
				}
				cc.lastTick = tick
				s.cnt.bytesC2U.Add(uint64(res))
				cc.c2uOff, cc.c2uEnd = 0, int(res)
				post(func(sq *uring.SQE) {
					uring.PrepSend(sq, cc.upstreamFD, cc.c2uBuf[cc.c2uOff:cc.c2uEnd], 0, ud(cc.id, opC2USend))
				})

			case opC2USend:
				cc := conns[cid]
				if cc == nil || cc.closing {
					break
				}
				if res <= 0 {
					closeConn(cc)
					break
				}
				cc.c2uOff += int(res)
				if cc.c2uOff < cc.c2uEnd {
					post(func(sq *uring.SQE) {
						uring.PrepSend(sq, cc.upstreamFD, cc.c2uBuf[cc.c2uOff:cc.c2uEnd], 0, ud(cc.id, opC2USend))
					})
					break
				}
				if !cc.clientReadDone {
					post(func(sq *uring.SQE) { uring.PrepRecv(sq, cc.clientFD, cc.c2uBuf, ud(cc.id, opC2URecv)) })
				}

			case opU2CRecv:
				cc := conns[cid]
				if cc == nil || cc.closing {
					break
				}
				if res < 0 {
					closeConn(cc)
					break
				}
				if res == 0 {
					syscall.Shutdown(cc.clientFD, syscall.SHUT_WR)
					cc.upstreamReadDone = true
					if cc.clientReadDone {
						closeConn(cc)
					}
					break
				}
				cc.lastTick = tick
				s.cnt.bytesU2C.Add(uint64(res))
				cc.u2cOff, cc.u2cEnd = 0, int(res)
				post(func(sq *uring.SQE) {
					uring.PrepSend(sq, cc.clientFD, cc.u2cBuf[cc.u2cOff:cc.u2cEnd], 0, ud(cc.id, opU2CSend))
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
				if cc.u2cOff < cc.u2cEnd {
					post(func(sq *uring.SQE) {
						uring.PrepSend(sq, cc.clientFD, cc.u2cBuf[cc.u2cOff:cc.u2cEnd], 0, ud(cc.id, opU2CSend))
					})
					break
				}
				// A completed upstream->client send means the relay forwarded a full
				// chunk; count it as a "completed" relay only at teardown. Re-arm read.
				if !cc.upstreamReadDone {
					post(func(sq *uring.SQE) { uring.PrepRecv(sq, cc.upstreamFD, cc.u2cBuf, ud(cc.id, opU2CRecv)) })
				}

			case opClose:
				cc := conns[cid]
				if cc == nil {
					break
				}
				cc.closesLeft--
				if cc.closesLeft <= 0 {
					s.cnt.completed.Add(1)
					delete(conns, cid)
				}

			case opTimeout:
				postTimeout()
				tick++
				// idle sweep ~every 1s (every 10th 100ms fire)
				if idleTicks > 0 && tick%10 == 0 {
					for _, cc := range conns {
						if !cc.closing && tick-cc.lastTick > idleTicks {
							s.cnt.idleClosed.Add(1)
							closeConn(cc)
						}
					}
				}
			}
		}
		ring.CQAdvance(n)

		// Drain on Stop: stop accepting; exit once all conns are gone.
		if s.stop.Load() {
			if len(conns) == 0 {
				return
			}
		} else {
			for acceptInflight < cfg.AcceptBatch && len(conns)+acceptInflight < cfg.MaxConns {
				postAccept()
			}
		}
	}
}
