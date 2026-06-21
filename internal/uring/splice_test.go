//go:build linux && amd64

package uring

import (
	"syscall"
	"testing"
)

// drive submits queued SQEs and waits for exactly one CQE, returning its res.
func drive(t *testing.T, r *Ring) int32 {
	t.Helper()
	if _, err := r.Submit(1); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	for r.CQReady() == 0 {
		if _, err := r.Submit(1); err != nil {
			t.Fatalf("Submit(wait): %v", err)
		}
	}
	res := r.PeekCQE(0).Res
	r.CQAdvance(1)
	return res
}

// TestSplice proves PrepSplice moves bytes socket→pipe→socket — exactly the
// relay's zero-copy path. Two socketpairs model the client side (src) and the
// upstream side (dst); a pipe is the kernel buffer between them. Data written to
// src[0] must arrive at dst[1] after two splices, all driven through the ring.
func TestSplice(t *testing.T) {
	r, err := New(8)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	src, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair src: %v", err)
	}
	dst, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair dst: %v", err)
	}
	var pipe [2]int
	if err := syscall.Pipe(pipe[:]); err != nil {
		t.Fatalf("pipe: %v", err)
	}
	for _, fd := range []int{src[0], src[1], dst[0], dst[1], pipe[0], pipe[1]} {
		defer syscall.Close(fd)
	}

	msg := []byte("hello-splice")
	if _, err := syscall.Write(src[0], msg); err != nil {
		t.Fatalf("write src: %v", err)
	}

	// splice: src[1] (recv end) -> pipe write end
	PrepSplice(r.GetSQE(), src[1], SpliceOffUnspecified, pipe[1], SpliceOffUnspecified, uint32(len(msg)), SpliceFMove, 1)
	if n := drive(t, r); n != int32(len(msg)) {
		t.Fatalf("splice sock->pipe: res=%d want %d", n, len(msg))
	}
	// splice: pipe read end -> dst[0] (send end)
	PrepSplice(r.GetSQE(), pipe[0], SpliceOffUnspecified, dst[0], SpliceOffUnspecified, uint32(len(msg)), SpliceFMove, 2)
	if n := drive(t, r); n != int32(len(msg)) {
		t.Fatalf("splice pipe->sock: res=%d want %d", n, len(msg))
	}

	got := make([]byte, len(msg))
	n, err := syscall.Read(dst[1], got)
	if err != nil || n != len(msg) || string(got[:n]) != string(msg) {
		t.Fatalf("dst read: n=%d got=%q err=%v want %q", n, got[:n], err, msg)
	}
	t.Logf("spliced %q socket->pipe->socket through the ring", got[:n])
}
