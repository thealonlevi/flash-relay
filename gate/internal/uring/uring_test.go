//go:build linux && amd64

package uring

import (
	"bytes"
	"runtime"
	"syscall"
	"testing"
)

// waitOne submits and harvests exactly one completion, returning its res.
func waitOne(t *testing.T, r *Ring) int32 {
	t.Helper()
	if _, err := r.Submit(1); err != nil {
		t.Fatalf("submit: %v", err)
	}
	for r.CQReady() == 0 {
		if _, err := r.Submit(1); err != nil {
			t.Fatalf("submit/wait: %v", err)
		}
	}
	res := r.PeekCQE(0).Res
	r.CQAdvance(1)
	return res
}

// TestRecvSend drives recv and send through the ring on a socketpair, proving
// the SQ/CQ protocol, mmap layout, and op prep are correct.
func TestRecvSend(t *testing.T) {
	r, err := New(8)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])

	// peer writes; ring recvs.
	want := []byte("flash-relay ring smoke test")
	if _, err := syscall.Write(fds[1], want); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	buf := make([]byte, 64)
	PrepRecv(r.GetSQE(), fds[0], buf, 1)
	n := waitOne(t, r)
	if int(n) != len(want) {
		t.Fatalf("recv res=%d want %d", n, len(want))
	}
	if !bytes.Equal(buf[:n], want) {
		t.Fatalf("recv payload mismatch: %q", buf[:n])
	}

	// ring sends; peer reads.
	reply := []byte("pong")
	PrepSend(r.GetSQE(), fds[0], reply, 0, 2)
	n = waitOne(t, r)
	if int(n) != len(reply) {
		t.Fatalf("send res=%d want %d", n, len(reply))
	}
	got := make([]byte, 16)
	rn, err := syscall.Read(fds[1], got)
	if err != nil {
		t.Fatalf("peer read: %v", err)
	}
	if !bytes.Equal(got[:rn], reply) {
		t.Fatalf("peer got %q want %q", got[:rn], reply)
	}
	runtime.KeepAlive(buf)
	runtime.KeepAlive(reply)
}

// TestAccept proves the accept op: post an accept on a loopback listener, have a
// blocking dialer connect, and confirm we get a valid client fd.
func TestAccept(t *testing.T) {
	r, err := New(8)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	lfd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	defer syscall.Close(lfd)
	if err := syscall.Bind(lfd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := syscall.Listen(lfd, 16); err != nil {
		t.Fatalf("listen: %v", err)
	}
	sa, err := syscall.Getsockname(lfd)
	if err != nil {
		t.Fatalf("getsockname: %v", err)
	}
	port := sa.(*syscall.SockaddrInet4).Port

	PrepAccept(r.GetSQE(), lfd, 99)
	if _, err := r.Submit(0); err != nil { // publish the accept before dialing
		t.Fatalf("submit accept: %v", err)
	}

	dialDone := make(chan int, 1)
	go func() {
		cfd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, syscall.IPPROTO_TCP)
		if err != nil {
			dialDone <- -1
			return
		}
		if err := syscall.Connect(cfd, &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
			dialDone <- -1
			return
		}
		dialDone <- cfd
	}()

	for r.CQReady() == 0 {
		if _, err := r.Submit(1); err != nil {
			t.Fatalf("submit/wait accept: %v", err)
		}
	}
	cqe := r.PeekCQE(0)
	if cqe.UserData != 99 {
		t.Fatalf("accept user_data=%d want 99", cqe.UserData)
	}
	if cqe.Res < 0 {
		t.Fatalf("accept failed: errno %d", -cqe.Res)
	}
	r.CQAdvance(1)
	clientFD := int(cqe.Res)
	defer syscall.Close(clientFD)

	dfd := <-dialDone
	if dfd < 0 {
		t.Fatal("dial failed")
	}
	defer syscall.Close(dfd)
}
