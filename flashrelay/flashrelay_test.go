//go:build linux && amd64

package flashrelay_test

import (
	"bytes"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/thealonlevi/flash-relay/flashrelay"
	"github.com/thealonlevi/flash-relay/internal/rawsock"
)

// echoUpstream starts a net echo server (the relay's upstream). Returns host/port.
func echoUpstream(t *testing.T) (host string, port int, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("upstream listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { io.Copy(c, c); c.Close() }(c)
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port, func() { ln.Close() }
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeport: %v", err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

func waitListen(t *testing.T, port int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("relay never came up on :%d", port)
}

// TestRelayEcho exercises the real library path: a Hook that dials the upstream
// with a blocking raw syscall (no netpoller) and returns its fd; the engine
// adopts it and relays bidirectionally. Verifies data integrity + clean drain.
func TestRelayEcho(t *testing.T) {
	upHost, upPort, upStop := echoUpstream(t)
	defer upStop()
	port := freePort(t)

	hook := func(req []byte, peer netip.AddrPort) flashrelay.Decision {
		fd, err := rawsock.Dial(upHost, upPort) // blocking raw dial — never the netpoller
		if err != nil {
			return flashrelay.Decision{Reject: true}
		}
		return flashrelay.Decision{UpstreamFD: fd}
	}
	srv, err := flashrelay.New(flashrelay.Config{
		Addr: "127.0.0.1", Port: port, Workers: 2, InitialReqLen: 64, BufSize: 4096,
	}, hook)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Run() }()
	waitListen(t, port)

	const N = 40
	var wg sync.WaitGroup
	var okc, failc int
	var mu sync.Mutex
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			init := bytes.Repeat([]byte{'I'}, 64)
			payload := make([]byte, 3000)
			for j := range payload {
				payload[j] = byte((j*7 + i) % 256)
			}
			want := append(append([]byte{}, init...), payload...)
			c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 3*time.Second)
			if err != nil {
				mu.Lock()
				failc++
				mu.Unlock()
				return
			}
			c.SetDeadline(time.Now().Add(5 * time.Second))
			c.Write(init)
			for o := 0; o < len(payload); o += 256 {
				e := o + 256
				if e > len(payload) {
					e = len(payload)
				}
				c.Write(payload[o:e])
			}
			c.(*net.TCPConn).CloseWrite() // half-close
			got := make([]byte, 0, len(want))
			buf := make([]byte, 4096)
			for len(got) < len(want) {
				n, err := c.Read(buf)
				got = append(got, buf[:n]...)
				if err != nil {
					break
				}
			}
			c.Close()
			mu.Lock()
			if bytes.Equal(got, want) {
				okc++
			} else {
				failc++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if okc != N {
		t.Fatalf("relay echo: %d/%d ok, %d failed", okc, N, failc)
	}
	st := srv.Stat()
	t.Logf("stats: accepted=%d completed=%d bytesC2U=%d bytesU2C=%d", st.Accepted, st.Completed, st.BytesC2U, st.BytesU2C)

	srv.Stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Stop (drain hang)")
	}
}

// TestRelayReject verifies the reject path: the hook denies, the engine sends the
// reply bytes and closes without relaying.
func TestRelayReject(t *testing.T) {
	port := freePort(t)
	hook := func(req []byte, peer netip.AddrPort) flashrelay.Decision {
		return flashrelay.Decision{Reject: true, Reply: []byte("DENIED")}
	}
	srv, _ := flashrelay.New(flashrelay.Config{Addr: "127.0.0.1", Port: port, Workers: 1, InitialReqLen: 8}, hook)
	done := make(chan error, 1)
	go func() { done <- srv.Run() }()
	waitListen(t, port)

	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte("hello123")) // 8 bytes -> triggers the hook
	got, _ := io.ReadAll(c)
	c.Close()
	if string(got) != "DENIED" {
		t.Fatalf("reject: got %q, want DENIED", got)
	}
	if r := srv.Stat().Rejected; r != 1 {
		t.Fatalf("rejected counter = %d, want 1", r)
	}
	srv.Stop()
	<-done
}

// TestIdleTimeout verifies the engine closes connections idle past IdleTimeout.
func TestIdleTimeout(t *testing.T) {
	upHost, upPort, upStop := echoUpstream(t)
	defer upStop()
	port := freePort(t)
	hook := func(req []byte, peer netip.AddrPort) flashrelay.Decision {
		fd, err := rawsock.Dial(upHost, upPort)
		if err != nil {
			return flashrelay.Decision{Reject: true}
		}
		return flashrelay.Decision{UpstreamFD: fd}
	}
	srv, _ := flashrelay.New(flashrelay.Config{
		Addr: "127.0.0.1", Port: port, Workers: 1, InitialReqLen: 4, IdleTimeout: 300 * time.Millisecond,
	}, hook)
	done := make(chan error, 1)
	go func() { done <- srv.Run() }()
	waitListen(t, port)
	defer func() { srv.Stop(); <-done }()

	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.Write([]byte("ping"))         // establish (relay forwards; upstream echoes "ping")
	io.ReadFull(c, make([]byte, 4)) // read the echo back -> relay is established + now idle
	// now idle; the idle sweep (~1s cadence) should close it within ~2s
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _ := c.Read(make([]byte, 16)) // should return 0 (EOF) when the engine closes us
	if n != 0 {
		t.Fatalf("expected idle close (EOF), got %d bytes", n)
	}
	if ic := srv.Stat().IdleClosed; ic < 1 {
		t.Fatalf("IdleClosed = %d, want >= 1", ic)
	}
}
