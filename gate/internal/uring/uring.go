//go:build linux && amd64

// Package uring is a minimal, hand-rolled io_uring binding for the flash-relay
// gate — pure Go, CGO_ENABLED=0, no third-party dependency. We own the ring so
// the auto-optimizer can mutate the hot path (RELAY_PLAN.md substrate decision),
// and so no data-plane fd can be contaminated by a library that secretly touches
// the Go netpoller.
//
// Scope for the gate: a single ring driven by one goroutine (the per-core worker
// LockOSThread'd by the caller). Single-shot ops only: accept, recv, send, close.
// No multishot, no registered files, no SQPOLL — those are post-gate (Step 4).
//
// ABI reference (read for layout/barrier ordering, depended on for nothing):
// liburing, godzie44/go-uring, pawelgaczynski/gain. Memory ordering: the SQ/CQ
// rings are a single-producer/single-consumer shared memory protocol with the
// kernel; we publish the SQ tail with a release store and read the CQ tail with
// an acquire load. Go's sync/atomic on the mmap'd words provides both (and at
// least the strength of the kernel's smp_store_release/smp_load_acquire).
package uring

import (
	"fmt"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// syscall numbers (linux/amd64).
const (
	sysIoUringSetup    = 425
	sysIoUringEnter    = 426
	sysIoUringRegister = 427
)

// mmap offsets.
const (
	offSQRing = 0
	offCQRing = 0x8000000
	offSQEs   = 0x10000000
)

// io_uring_params.features bits.
const featSingleMmap = 1 // IORING_FEAT_SINGLE_MMAP

// io_uring_enter flags.
const enterGetevents = 1 // IORING_ENTER_GETEVENTS

// accept_flags (sqe.ioprio) and cqe.flags bits used for multishot accept.
const (
	acceptMultishot = 1 << 0 // IORING_ACCEPT_MULTISHOT (set in sqe.ioprio)
	CQEFMore        = 1 << 1 // IORING_CQE_F_MORE: the SQE will yield more CQEs
)

// Opcodes (subset used by the gate).
const (
	opNop     = 0
	OpTimeout = 11
	OpAccept  = 13
	OpConnect = 16
	OpClose   = 19
	OpRead    = 22
	OpSend    = 26
	OpRecv    = 27
)

// Timespec mirrors struct __kernel_timespec (for OpTimeout).
type Timespec struct {
	Sec  int64
	Nsec int64
}

// SQE mirrors struct io_uring_sqe (64 bytes). Field offsets are load-bearing —
// do not reorder.
type SQE struct {
	Opcode      uint8  // 0
	Flags       uint8  // 1
	IoPrio      uint16 // 2
	Fd          int32  // 4
	Off         uint64 // 8   (off / addr2)
	Addr        uint64 // 16  (addr / splice_off_in)
	Len         uint32 // 24
	OpFlags     uint32 // 28  (rw_flags / msg_flags / accept_flags / ...)
	UserData    uint64 // 32
	BufIndex    uint16 // 40  (buf_index / buf_group)
	Personality uint16 // 42
	SpliceFdIn  int32  // 44  (splice_fd_in / file_index)
	Addr3       uint64 // 48
	_pad2       uint64 // 56
}

// CQE mirrors struct io_uring_cqe (16 bytes).
type CQE struct {
	UserData uint64 // 0
	Res      int32  // 8
	Flags    uint32 // 12
}

// io_uring_params and the ring offset structs.
type sqRingOffsets struct {
	head, tail, ringMask, ringEntries, flags, dropped, array, resv1 uint32
	resv2                                                           uint64
}

type cqRingOffsets struct {
	head, tail, ringMask, ringEntries, overflow, cqes, flags, resv1 uint32
	resv2                                                           uint64
}

type params struct {
	sqEntries    uint32
	cqEntries    uint32
	flags        uint32
	sqThreadCPU  uint32
	sqThreadIdle uint32
	features     uint32
	wqFd         uint32
	resv         [3]uint32
	sqOff        sqRingOffsets
	cqOff        cqRingOffsets
}

// Ring is one io_uring instance. Not safe for concurrent use by multiple
// goroutines — drive it from a single worker goroutine.
type Ring struct {
	fd int

	sqRing  []byte
	cqRing  []byte // aliases sqRing when the kernel reports single-mmap
	sqeMmap []byte

	sqes    []SQE
	sqArray []uint32
	cqes    []CQE

	sqHead *uint32
	sqTail *uint32
	cqHead *uint32
	cqTail *uint32

	sqMask    uint32
	cqMask    uint32
	sqEntries uint32
	cqEntries uint32

	// userspace shadows
	sqTailLocal uint32
	cqHeadLocal uint32
	toSubmit    uint32

	singleMmap bool
}

func ptr32(b []byte, off uint32) *uint32 {
	return (*uint32)(unsafe.Pointer(&b[off]))
}

// New creates a ring sized for at least `entries` SQEs (rounded up to a power of
// two by the kernel).
func New(entries uint32) (*Ring, error) {
	var p params
	r1, _, errno := syscall.Syscall(sysIoUringSetup, uintptr(entries),
		uintptr(unsafe.Pointer(&p)), 0)
	if errno != 0 {
		return nil, fmt.Errorf("io_uring_setup: %w", errno)
	}
	ring := &Ring{fd: int(r1), sqEntries: p.sqEntries, cqEntries: p.cqEntries}
	ring.singleMmap = p.features&featSingleMmap != 0

	sqRingSz := p.sqOff.array + p.sqEntries*4         // array is u32 indices
	cqRingSz := p.cqOff.cqes + p.cqEntries*16         // cqes are 16 bytes each
	if ring.singleMmap {
		if cqRingSz > sqRingSz {
			sqRingSz = cqRingSz
		}
		cqRingSz = sqRingSz
	}

	var err error
	ring.sqRing, err = mmap(ring.fd, offSQRing, int(sqRingSz))
	if err != nil {
		_ = syscall.Close(ring.fd)
		return nil, fmt.Errorf("mmap sq ring: %w", err)
	}
	if ring.singleMmap {
		ring.cqRing = ring.sqRing
	} else {
		ring.cqRing, err = mmap(ring.fd, offCQRing, int(cqRingSz))
		if err != nil {
			ring.Close()
			return nil, fmt.Errorf("mmap cq ring: %w", err)
		}
	}
	ring.sqeMmap, err = mmap(ring.fd, offSQEs, int(p.sqEntries)*int(unsafe.Sizeof(SQE{})))
	if err != nil {
		ring.Close()
		return nil, fmt.Errorf("mmap sqes: %w", err)
	}

	ring.sqes = unsafe.Slice((*SQE)(unsafe.Pointer(&ring.sqeMmap[0])), p.sqEntries)
	ring.sqArray = unsafe.Slice(ptr32(ring.sqRing, p.sqOff.array), p.sqEntries)
	ring.cqes = unsafe.Slice((*CQE)(unsafe.Pointer(&ring.cqRing[p.cqOff.cqes])), p.cqEntries)

	ring.sqHead = ptr32(ring.sqRing, p.sqOff.head)
	ring.sqTail = ptr32(ring.sqRing, p.sqOff.tail)
	ring.cqHead = ptr32(ring.cqRing, p.cqOff.head)
	ring.cqTail = ptr32(ring.cqRing, p.cqOff.tail)
	ring.sqMask = *ptr32(ring.sqRing, p.sqOff.ringMask)
	ring.cqMask = *ptr32(ring.cqRing, p.cqOff.ringMask)

	ring.sqTailLocal = atomic.LoadUint32(ring.sqTail)
	ring.cqHeadLocal = atomic.LoadUint32(ring.cqHead)
	return ring, nil
}

// FD returns the ring file descriptor (for io_uring_register, etc.).
func (r *Ring) FD() int { return r.fd }

// GetSQE returns the next free submission queue entry (zeroed), or nil if the
// SQ is full. The returned SQE is valid until the next Submit.
func (r *Ring) GetSQE() *SQE {
	head := atomic.LoadUint32(r.sqHead) // acquire: kernel-advanced consumer head
	if r.sqTailLocal-head >= r.sqEntries {
		return nil // SQ full
	}
	idx := r.sqTailLocal & r.sqMask
	sqe := &r.sqes[idx]
	*sqe = SQE{}
	r.sqArray[idx] = idx
	r.sqTailLocal++
	r.toSubmit++
	return sqe
}

// Submit publishes prepared SQEs and, if waitNr > 0, blocks until at least
// waitNr completions are available. Returns the number of SQEs consumed.
func (r *Ring) Submit(waitNr uint32) (int, error) {
	atomic.StoreUint32(r.sqTail, r.sqTailLocal) // release: publish tail to kernel
	var flags uintptr
	if waitNr > 0 {
		flags = enterGetevents
	}
	for {
		r1, _, errno := syscall.Syscall6(sysIoUringEnter, uintptr(r.fd),
			uintptr(r.toSubmit), uintptr(waitNr), flags, 0, 0)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return 0, fmt.Errorf("io_uring_enter: %w", errno)
		}
		r.toSubmit = 0
		return int(r1), nil
	}
}

// CQReady returns the number of completions ready to harvest.
func (r *Ring) CQReady() uint32 {
	return atomic.LoadUint32(r.cqTail) - r.cqHeadLocal // acquire on cqTail
}

// PeekCQE returns the i-th unconsumed completion (0 == oldest). Call only with
// i < CQReady(). The pointer is valid until CQAdvance.
func (r *Ring) PeekCQE(i uint32) *CQE {
	return &r.cqes[(r.cqHeadLocal+i)&r.cqMask]
}

// CQAdvance marks n completions consumed, freeing those CQ slots for the kernel.
func (r *Ring) CQAdvance(n uint32) {
	r.cqHeadLocal += n
	atomic.StoreUint32(r.cqHead, r.cqHeadLocal) // release: publish consumer head
}

// Close munmaps the rings and closes the ring fd.
func (r *Ring) Close() {
	if r.sqeMmap != nil {
		_ = munmap(r.sqeMmap)
	}
	if !r.singleMmap && r.cqRing != nil {
		_ = munmap(r.cqRing)
	}
	if r.sqRing != nil {
		_ = munmap(r.sqRing)
	}
	if r.fd != 0 {
		_ = syscall.Close(r.fd)
	}
}

// --- op prep helpers -------------------------------------------------------
//
// Buffers passed to Prep* are read/written by the kernel asynchronously; the
// caller MUST keep them alive (and not let the GC reclaim them) until the
// matching CQE is harvested. Keep them in per-connection state.

// PrepAccept prepares a single-shot accept on listenFd. addr/addrLen may be nil
// (use Accept4-style; here we pass 0 to skip peer address capture at the gate).
func PrepAccept(s *SQE, listenFd int, ud uint64) {
	s.Opcode = OpAccept
	s.Fd = int32(listenFd)
	s.UserData = ud
}

// PrepAcceptMultishot prepares a multishot accept: one armed SQE yields a CQE
// per accepted connection and stays active (no per-conn re-arm) as long as each
// CQE carries CQEFMore. Re-arm only when CQEFMore is absent (multishot ended).
func PrepAcceptMultishot(s *SQE, listenFd int, ud uint64) {
	s.Opcode = OpAccept
	s.Fd = int32(listenFd)
	s.IoPrio = acceptMultishot
	s.UserData = ud
}

// PrepRead prepares a read(2) into buf at offset 0 (used for the eventfd wakeup
// bridge that lets off-ring hook goroutines wake the ring worker).
func PrepRead(s *SQE, fd int, buf []byte, ud uint64) {
	s.Opcode = OpRead
	s.Fd = int32(fd)
	s.Addr = uint64(uintptr(unsafe.Pointer(&buf[0])))
	s.Len = uint32(len(buf))
	s.UserData = ud
}

// PrepRecv prepares a recv into buf.
func PrepRecv(s *SQE, fd int, buf []byte, ud uint64) {
	s.Opcode = OpRecv
	s.Fd = int32(fd)
	s.Addr = uint64(uintptr(unsafe.Pointer(&buf[0])))
	s.Len = uint32(len(buf))
	s.UserData = ud
}

// PrepSend prepares a send of buf. flags maps to send(2) msg_flags (e.g.
// MSG_MORE) via OpFlags.
func PrepSend(s *SQE, fd int, buf []byte, flags uint32, ud uint64) {
	s.Opcode = OpSend
	s.Fd = int32(fd)
	s.Addr = uint64(uintptr(unsafe.Pointer(&buf[0])))
	s.Len = uint32(len(buf))
	s.OpFlags = flags
	s.UserData = ud
}

// PrepClose prepares a close of fd.
func PrepClose(s *SQE, fd int, ud uint64) {
	s.Opcode = OpClose
	s.Fd = int32(fd)
	s.UserData = ud
}

// PrepTimeout prepares a relative timeout that completes (res = -ETIME) after
// ts elapses. Keep it always armed so the ring worker can never block forever in
// io_uring_enter waiting for a completion that won't come (the flood deadlock).
// ts must stay alive until the CQE (kernel reads it asynchronously).
func PrepTimeout(s *SQE, ts *Timespec, ud uint64) {
	s.Opcode = OpTimeout
	s.Fd = -1
	s.Addr = uint64(uintptr(unsafe.Pointer(ts)))
	s.Len = 1 // one timespec
	s.Off = 0 // count=0: fire purely on the timeout
	s.UserData = ud
}

func mmap(fd int, offset int64, length int) ([]byte, error) {
	return syscall.Mmap(fd, offset, length,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED|syscall.MAP_POPULATE)
}

func munmap(b []byte) error { return syscall.Munmap(b) }
