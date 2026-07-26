//go:build linux && amd64

package flashrelay_test

import (
	"bytes"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thealonlevi/flash-relay/flashrelay"
	"github.com/thealonlevi/flash-relay/internal/rawsock"
)

// socketCount reports this process's open SOCKET fds. The engine runs in the test
// binary, so an upstream fd it forgot to close shows up here.
//
// Sockets specifically, not all fds: the Go runtime spawns threads (and pipe
// pairs) when the hook goroutines enter blocking connects, so a raw fd count
// drifts for reasons that have nothing to do with the engine.
func socketCount(t *testing.T) int {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("/proc/self/fd unavailable: %v", err)
	}
	n := 0
	for _, e := range ents {
		if l, err := os.Readlink("/proc/self/fd/" + e.Name()); err == nil && strings.HasPrefix(l, "socket:") {
			n++
		}
	}
	return n
}

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
	// Each conn pushes 64 init + 3000 payload = 3064 bytes up; the echo upstream
	// returns the same down. The counters must account for both directions in
	// full, INCLUDING the forwarded initial request (regression: that used to be
	// omitted from BytesC2U).
	const perConn = 64 + 3000
	if want := uint64(N * perConn); st.BytesC2U != want || st.BytesU2C != want {
		t.Fatalf("byte counters: C2U=%d U2C=%d, want %d each", st.BytesC2U, st.BytesU2C, want)
	}

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

// TestHookMoreContinuation covers Decision.More. A client request can span
// several TCP segments, so one read is not a message boundary — the Hook must be
// able to ask for the rest instead of deciding on a truncated handshake (the
// SOCKS5 / HTTP-CONNECT case). InitialReqLen is tiny here to force several
// rounds; the accumulated buffer must grow and carry ALL the bytes.
func TestHookMoreContinuation(t *testing.T) {
	upHost, upPort, upStop := echoUpstream(t)
	defer upStop()
	port := freePort(t)

	var rounds atomic.Int32
	var sawFull atomic.Bool
	hook := func(req []byte, peer netip.AddrPort) flashrelay.Decision {
		rounds.Add(1)
		if !bytes.Contains(req, []byte("\n")) {
			return flashrelay.Decision{More: true} // partial handshake: read on
		}
		if string(req) == "HANDSHAKE!\n" {
			sawFull.Store(true)
		}
		fd, err := rawsock.Dial(upHost, upPort)
		if err != nil {
			return flashrelay.Decision{Reject: true}
		}
		return flashrelay.Decision{UpstreamFD: fd} // Consumed 0: forward it all
	}
	srv, err := flashrelay.New(flashrelay.Config{
		Addr: "127.0.0.1", Port: port, Workers: 1, InitialReqLen: 2, BufSize: 4096,
	}, hook)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Run() }()
	waitListen(t, port)
	defer func() { srv.Stop(); <-done }()

	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	for _, seg := range []string{"HAND", "SHAKE", "!\n"} { // three distinct segments
		c.Write([]byte(seg))
		time.Sleep(60 * time.Millisecond)
	}
	const want = "HANDSHAKE!\n"
	got := make([]byte, len(want))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read back: %v (got %q)", err, got)
	}
	if string(got) != want {
		t.Fatalf("relayed %q, want %q", got, want)
	}
	if r := rounds.Load(); r < 2 {
		t.Fatalf("hook ran %d time(s) — More was never exercised", r)
	}
	if !sawFull.Load() {
		t.Fatal("hook never saw the complete accumulated handshake")
	}
}

// TestHookConsumedNotForwarded covers Decision.Consumed: handshake bytes the Hook
// terminates must NOT reach the upstream. Without it a SOCKS5/CONNECT relay would
// forward its own handshake into the tunnel and corrupt every connection.
func TestHookConsumedNotForwarded(t *testing.T) {
	upHost, upPort, upStop := echoUpstream(t)
	defer upStop()
	port := freePort(t)

	hook := func(req []byte, peer netip.AddrPort) flashrelay.Decision {
		i := bytes.IndexByte(req, '\n')
		if i < 0 {
			return flashrelay.Decision{More: true}
		}
		fd, err := rawsock.Dial(upHost, upPort)
		if err != nil {
			return flashrelay.Decision{Reject: true}
		}
		return flashrelay.Decision{UpstreamFD: fd, Consumed: i + 1} // swallow "AUTH\n"
	}
	srv, _ := flashrelay.New(flashrelay.Config{
		Addr: "127.0.0.1", Port: port, Workers: 1, InitialReqLen: 64, BufSize: 4096,
	}, hook)
	done := make(chan error, 1)
	go func() { done <- srv.Run() }()
	waitListen(t, port)

	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("AUTH\npayload"))
	got := make([]byte, len("payload"))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("read back: %v (got %q)", err, got)
	}
	if string(got) != "payload" {
		t.Fatalf("upstream echoed %q — the consumed handshake was forwarded", got)
	}
	c.Close()
	srv.Stop()
	<-done
	// Only the unconsumed bytes are relayed traffic.
	if st := srv.Stat(); st.BytesC2U != uint64(len("payload")) {
		t.Fatalf("BytesC2U = %d, want %d (consumed handshake must not be counted)", st.BytesC2U, len("payload"))
	}
}

// TestHookMoreExceedsMaxReqLen verifies the accumulated request buffer is bounded.
// A Hook that keeps asking for More must not be able to grow a per-connection
// buffer without limit — that would be a memory amplification vector under the
// exact connect-flood this engine exists to survive.
func TestHookMoreExceedsMaxReqLen(t *testing.T) {
	port := freePort(t)
	hook := func(req []byte, peer netip.AddrPort) flashrelay.Decision {
		return flashrelay.Decision{More: true} // never satisfied
	}
	srv, _ := flashrelay.New(flashrelay.Config{
		Addr: "127.0.0.1", Port: port, Workers: 1, InitialReqLen: 4, MaxReqLen: 8,
	}, hook)
	done := make(chan error, 1)
	go func() { done <- srv.Run() }()
	waitListen(t, port)

	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write(bytes.Repeat([]byte{'x'}, 64))
	if n, _ := c.Read(make([]byte, 16)); n != 0 { // engine must close us
		t.Fatalf("expected close at the MaxReqLen cap, read %d bytes", n)
	}
	c.Close()
	srv.Stop()
	<-done
	st := srv.Stat()
	if st.Errors < 1 {
		t.Fatalf("Errors = %d, want >= 1 (cap breach must be charged)", st.Errors)
	}
	if st.Completed != 0 {
		t.Fatalf("Completed = %d, want 0 (connection never relayed)", st.Completed)
	}
}

// TestRejectWithUpstreamFDNoLeak: a Hook that returns Reject while also handing
// back a dialed upstream fd (a caller bug, but one that happens when auth fails
// after the dial) must not leak that fd. Regression: the engine used to skip the
// cleanup close whenever Reject was set.
func TestRejectWithUpstreamFDNoLeak(t *testing.T) {
	upHost, upPort, upStop := echoUpstream(t)
	defer upStop()
	port := freePort(t)

	hook := func(req []byte, peer netip.AddrPort) flashrelay.Decision {
		fd, err := rawsock.Dial(upHost, upPort)
		if err != nil {
			return flashrelay.Decision{Reject: true}
		}
		return flashrelay.Decision{Reject: true, Reply: []byte("NO"), UpstreamFD: fd}
	}
	srv, _ := flashrelay.New(flashrelay.Config{
		Addr: "127.0.0.1", Port: port, Workers: 1, InitialReqLen: 4,
	}, hook)
	done := make(chan error, 1)
	go func() { done <- srv.Run() }()
	waitListen(t, port)

	const N = 25
	base := socketCount(t)
	for i := 0; i < N; i++ {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 3*time.Second)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		c.SetDeadline(time.Now().Add(3 * time.Second))
		c.Write([]byte("ping"))
		io.ReadAll(c)
		c.Close()
	}
	srv.Stop()
	<-done
	// Small slack for upstream-side conns still winding down; a genuine leak is N.
	if grew := socketCount(t) - base; grew > 3 {
		t.Fatalf("socket count grew by %d over %d rejected conns — upstream fds leaked", grew, N)
	}
	if st := srv.Stat(); st.Rejected != N {
		t.Fatalf("Rejected = %d, want %d", st.Rejected, N)
	}
}

// TestSlowHookIdleCloseNoLeak drives the race the engine must survive on the
// integration path: the idle timeout tears a connection down while its Hook is
// still running (slow auth under a flood), and the late result arrives carrying a
// dialed upstream fd that now belongs to nobody. The engine must close that fd
// rather than adopt it onto a dead connection.
//
// Coverage, honestly: the conn-already-deleted case is hit every run. The
// narrower case — the result landing while the conn is still mid-teardown
// (closing, closesLeft > 0) — needs the hook result to be drained in the same CQE
// batch that the idle sweep closed the conn in, a window microseconds wide. The
// staggered hook durations below spread completions across the sweep cadence so
// runs hit it probabilistically; a single run may not. Both cases are guarded by
// the same cc.closing check, and a leak in either shows up in the socket count.
func TestSlowHookIdleCloseNoLeak(t *testing.T) {
	upHost, upPort, upStop := echoUpstream(t)
	defer upStop()
	port := freePort(t)

	var seq atomic.Int32
	hook := func(req []byte, peer netip.AddrPort) flashrelay.Decision {
		// Stagger across the ~1s idle-sweep cadence so completions land at every
		// phase relative to a sweep, including inside the teardown window.
		n := seq.Add(1)
		time.Sleep(time.Duration(1400+(int(n)*97)%1200) * time.Millisecond)
		fd, err := rawsock.Dial(upHost, upPort)
		if err != nil {
			return flashrelay.Decision{Reject: true}
		}
		return flashrelay.Decision{UpstreamFD: fd}
	}
	srv, _ := flashrelay.New(flashrelay.Config{
		Addr: "127.0.0.1", Port: port, Workers: 1, InitialReqLen: 4,
		IdleTimeout: 200 * time.Millisecond,
	}, hook)
	done := make(chan error, 1)
	go func() { done <- srv.Run() }()
	waitListen(t, port)

	const N = 24
	base := socketCount(t)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 3*time.Second)
			if err != nil {
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(8 * time.Second))
			c.Write([]byte("ping"))
			c.Read(make([]byte, 16)) // expect the idle close
		}()
	}
	wg.Wait()
	time.Sleep(3500 * time.Millisecond) // let every in-flight hook land
	srv.Stop()
	<-done
	if grew := socketCount(t) - base; grew > 3 {
		t.Fatalf("socket count grew by %d over %d idle-closed conns — late hook fds leaked", grew, N)
	}
	st := srv.Stat()
	if st.IdleClosed < 1 {
		t.Fatalf("IdleClosed = %d, want >= 1", st.IdleClosed)
	}
	if st.Completed != 0 {
		t.Fatalf("Completed = %d, want 0 — idle-closed conns must not count as completed", st.Completed)
	}
}

// TestStatsTerminalBuckets pins the counter partition: every accepted connection
// lands in exactly one terminal bucket. Regression: Completed used to increment
// on every close, so rejects and idle-closes inflated it.
func TestStatsTerminalBuckets(t *testing.T) {
	upHost, upPort, upStop := echoUpstream(t)
	defer upStop()
	port := freePort(t)

	hook := func(req []byte, peer netip.AddrPort) flashrelay.Decision {
		if bytes.HasPrefix(req, []byte("DENY")) {
			return flashrelay.Decision{Reject: true, Reply: []byte("NO")}
		}
		fd, err := rawsock.Dial(upHost, upPort)
		if err != nil {
			return flashrelay.Decision{Reject: true}
		}
		return flashrelay.Decision{UpstreamFD: fd}
	}
	srv, _ := flashrelay.New(flashrelay.Config{
		Addr: "127.0.0.1", Port: port, Workers: 1, InitialReqLen: 4, BufSize: 4096,
	}, hook)
	done := make(chan error, 1)
	go func() { done <- srv.Run() }()
	waitListen(t, port)

	const relayed, denied = 12, 8
	send := func(msg string, readBack int) {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 3*time.Second)
		if err != nil {
			t.Errorf("dial: %v", err)
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(4 * time.Second))
		c.Write([]byte(msg))
		io.ReadFull(c, make([]byte, readBack))
		c.(*net.TCPConn).CloseWrite()
		io.ReadAll(c)
	}
	for i := 0; i < relayed; i++ {
		send("PASS", 4) // echoed back by the upstream
	}
	for i := 0; i < denied; i++ {
		send("DENY", 2) // "NO"
	}
	srv.Stop()
	<-done

	// waitListen's probe connection is itself accepted (and dies pre-hook, so it
	// lands in Errors) — hence the >= rather than == on Accepted. The partition
	// identity below is the real assertion and accounts for it exactly.
	st := srv.Stat()
	if st.Accepted < relayed+denied {
		t.Fatalf("Accepted = %d, want >= %d", st.Accepted, relayed+denied)
	}
	if st.Rejected != denied {
		t.Fatalf("Rejected = %d, want %d", st.Rejected, denied)
	}
	if st.Completed != relayed {
		t.Fatalf("Completed = %d, want %d (rejects must not be counted as completed)", st.Completed, relayed)
	}
	sum := st.Completed + st.Rejected + st.Shed + st.IdleClosed + st.Errors + st.LiveConns
	if sum != st.Accepted {
		t.Fatalf("terminal buckets sum to %d, accepted %d (stats: %+v)", sum, st.Accepted, st)
	}
	if st.CQOverflow != 0 {
		t.Fatalf("CQOverflow = %d, want 0", st.CQOverflow)
	}
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
