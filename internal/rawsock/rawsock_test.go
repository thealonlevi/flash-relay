//go:build linux

package rawsock

import (
	"syscall"
	"testing"
)

// roundtrip: Listen + Dial on the given IP family, accept once, send a byte both ways.
func roundtrip(t *testing.T, ip string) {
	t.Helper()
	ln, err := Listen(ip, 0, 16)
	if err != nil {
		t.Fatalf("Listen(%q): %v", ip, err)
	}
	defer ln.Close()
	done := make(chan int, 1)
	go func() {
		afd, _, err := syscall.Accept(ln.FD)
		if err != nil {
			done <- -1
			return
		}
		done <- afd
	}()
	cfd, err := Dial(ip, ln.Port)
	if err != nil {
		t.Fatalf("Dial(%q): %v", ip, err)
	}
	defer syscall.Close(cfd)
	afd := <-done
	if afd < 0 {
		t.Fatalf("accept(%q) failed", ip)
	}
	defer syscall.Close(afd)
	if _, err := syscall.Write(cfd, []byte("x")); err != nil {
		t.Fatalf("write(%q): %v", ip, err)
	}
	b := make([]byte, 1)
	if n, err := syscall.Read(afd, b); err != nil || n != 1 || b[0] != 'x' {
		t.Fatalf("read(%q): n=%d b=%q err=%v", ip, n, b[:n], err)
	}
}

func TestListenDialIPv4(t *testing.T) { roundtrip(t, "127.0.0.1") }
func TestListenDialIPv6(t *testing.T) { roundtrip(t, "::1") }
