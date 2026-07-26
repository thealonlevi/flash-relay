# Deploying flash-relay on a new server

*From a bare Linux box to a running relay. flash-relay is a **library**, so "deploying
it" means building a service that embeds it — this guide does that end to end, starting
with the bundled example so you have something listening within a few minutes.*

---

## 1. Requirements

| | requirement | why |
|---|---|---|
| OS | **Linux, x86-64** | io_uring + raw syscalls; the build is constrained to `linux && amd64` |
| Kernel | **≥ 5.6** to run, **≥ 5.7** for the full test suite | the relay path needs `TIMEOUT` (5.4), `ACCEPT` (5.5), `SEND`/`RECV`/`READ`/`CLOSE` (5.6); the ring's splice test needs `SPLICE` (5.7). `IORING_FEAT_NODROP` arrived in 5.5 — the engine logs a warning without it |
| Go | **≥ 1.25** | see `go.mod` |
| cgo | **not needed** | pure Go; build with `CGO_ENABLED=0`. Only `go test -race` needs cgo |
| privileges | **none** to run the relay | root is needed only for the research rig (`perf`/`taskset`) and the fingerprint eBPF (`CAP_NET_ADMIN`) |

### Check the box before you build

```sh
uname -srm                                        # Linux ... x86_64, kernel >= 5.7
go version                                        # go1.25+
cat /proc/sys/kernel/io_uring_disabled 2>/dev/null || echo "sysctl absent (fine)"
```

`io_uring_disabled` exists on kernel 6.6+ and is the one setting that will stop this dead:

| value | meaning |
|---|---|
| `0` | io_uring available to everyone — what you want |
| `1` | only for processes with `CAP_SYS_ADMIN` — the relay must run privileged or the sysctl must be relaxed |
| `2` | **fully disabled** — `io_uring_setup(2)` returns `EPERM` and flash-relay cannot run at all |

If it is `2` (some hardened distros and container hosts default this way):

```sh
sudo sysctl -w kernel.io_uring_disabled=0
echo 'kernel.io_uring_disabled = 0' | sudo tee /etc/sysctl.d/60-io_uring.conf
```

> Containers: many runtimes block `io_uring_setup` via seccomp regardless of the sysctl.
> If you are deploying into Docker/Kubernetes, verify there first — a seccomp denial looks
> identical to a code failure. See §7.

---

## 2. Clone and build

```sh
git clone https://github.com/thealonlevi/flash-relay.git
cd flash-relay

CGO_ENABLED=0 go build ./...          # library + examples + research rig
CGO_ENABLED=0 go vet ./...
```

## 3. Verify the install

Run the real suite before trusting the box. It drives actual io_uring rings and loopback
sockets, so it *is* the environment check:

```sh
go test ./flashrelay/ ./internal/...      # ~9s
```

If this fails at ring setup (`io_uring_setup: operation not permitted`), the problem is
§1, not the code.

Then prove it end to end with the bundled example:

```sh
# terminal 1 — an upstream to relay to
nc -lk 127.0.0.1 9000

# terminal 2 — the relay
go run ./examples/echo-relay -addr 127.0.0.1 -port 8080 -upstream 127.0.0.1:9000

# terminal 3
printf 'hello\n' | nc 127.0.0.1 8080      # appears in terminal 1
```

Ctrl-C the relay: it drains in-flight connections and prints its counters.

---

## 4. Host it as a service

### 4a. Build a binary

```sh
CGO_ENABLED=0 go build -o /usr/local/bin/flash-relay ./examples/echo-relay
```

Replace `./examples/echo-relay` with your own service once you have one (§5) — the
example dials a single fixed upstream and authorizes nothing, so it is a smoke test and
a starting skeleton, **not** a production front end.

### 4b. systemd unit

`/etc/systemd/system/flash-relay.service`:

```ini
[Unit]
Description=flash-relay io_uring TCP relay
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/flash-relay -addr 0.0.0.0 -port 8443 -upstream 10.0.0.9:443 -workers 8
Restart=on-failure
RestartSec=2s

# Two fds per relayed connection (client + upstream), plus the ring and listener
# per worker. Size this ABOVE Workers x MaxConns x 2 -- see the sizing note in §6.
LimitNOFILE=1048576

# SIGTERM triggers a graceful drain; give it time to finish in-flight connections.
KillSignal=SIGTERM
TimeoutStopSec=30

User=flashrelay
Group=flashrelay
# Only needed to bind a port below 1024:
# AmbientCapabilities=CAP_NET_BIND_SERVICE

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true

[Install]
WantedBy=multi-user.target
```

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin flashrelay
sudo systemctl daemon-reload
sudo systemctl enable --now flash-relay
systemctl status flash-relay
journalctl -u flash-relay -f
```

> **Do not add `MemoryDenyWriteExecute=true` or a restrictive `SystemCallFilter=`** without
> testing — both can block `io_uring_setup`/`io_uring_enter` and the service will fail at
> startup with what looks like a code bug.

---

## 5. Embedding it in your own service

The example's `Hook` is the part you replace. It runs **off-ring on a goroutine pool**, so
it may block — do auth, blacklist lookups, IP allocation, and the upstream dial there.

```go
package main

import (
	"log"
	"net/netip"
	"time"

	"github.com/thealonlevi/flash-relay/flashrelay"
)

func main() {
	hook := func(req []byte, peer netip.AddrPort) flashrelay.Decision {
		// req = client bytes so far. One read is NOT a message boundary: return
		// More to be called again with the accumulated request.
		if !requestComplete(req) {
			return flashrelay.Decision{More: true}
		}
		user, host, port, n, ok := parse(req)
		if !ok || !authorize(user, peer) {
			return flashrelay.Decision{Reject: true, Reply: []byte("denied\n")}
		}
		// Blocking raw-syscall dial: never registers with the Go netpoller.
		fd, err := flashrelay.Dial(host, port)
		if err != nil {
			return flashrelay.Decision{Reject: true, Reply: []byte("upstream unavailable\n")}
		}
		// Consumed: handshake bytes WE terminated, so they are not forwarded upstream.
		return flashrelay.Decision{UpstreamFD: fd, Consumed: n}
	}

	srv, err := flashrelay.New(flashrelay.Config{
		Addr: "0.0.0.0", Port: 8443, Workers: 8,
		IdleTimeout: 5 * time.Minute,
	}, hook)
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		for range time.Tick(30 * time.Second) {
			st := srv.Stat()
			log.Printf("accepted=%d completed=%d rejected=%d live=%d cqoverflow=%d",
				st.Accepted, st.Completed, st.Rejected, st.LiveConns, st.CQOverflow)
		}
	}()
	log.Fatal(srv.Run()) // blocks; srv.Stop() drains and returns
}
```

Or consume it as a dependency instead of vendoring the repo:

```sh
go get github.com/thealonlevi/flash-relay/flashrelay
```

Full reference: [`API.md`](API.md) or `go doc ./flashrelay`.

---

## 6. Sizing and kernel tuning

### The two numbers that matter

**`MaxConns` is per worker, not global.** The total connection ceiling is
`Workers × MaxConns` (default `NumCPU() × 50000`), and **each relayed connection holds
two fds and two `BufSize` buffers** (default `2 × 16 KiB = 32 KiB`, allocated when the
connection starts relaying, not at accept).

So on a 16-core box at defaults the theoretical ceiling is 800k connections — about
**25 GiB of relay buffers and 1.6M fds**. Set these deliberately:

```go
Workers:  8,       // rings; one per core you intend to give it
MaxConns: 20000,   // per worker -> 160k total
BufSize:  16384,   // x2 per relayed conn -> ~5 GiB at 160k
```

and make `LimitNOFILE` exceed `Workers × MaxConns × 2` with headroom.

### Sysctls

```sh
# Listen backlog: the library requests 4096; the kernel silently clamps to somaxconn.
sudo sysctl -w net.core.somaxconn=4096

# Source ports for outbound dials -- a high-churn egress exhausts the default range.
sudo sysctl -w net.ipv4.ip_local_port_range="10240 65535"

# Optional: raise if you see accept queue overflows under load.
sudo sysctl -w net.ipv4.tcp_max_syn_backlog=8192
```

Persist them in `/etc/sysctl.d/60-flash-relay.conf`.

### What to watch in production

`Stat()` is the health surface. Every accepted connection lands in exactly one terminal
bucket, so `Accepted == Completed + Rejected + Shed + IdleClosed + errors + LiveConns`.

| signal | meaning |
|---|---|
| `CQOverflow > 0` | **hard fault** — completions dropped, connections stall and fds leak. Lower `MaxConns`/`AcceptBatch` or raise `RingSize` |
| `Shed` climbing | you are at the `MaxConns` backpressure cap |
| `Errors` climbing | abnormal teardowns (peer resets, failed dials, `MaxReqLen` breaches) |
| a startup log about `IORING_FEAT_NODROP` | kernel too old to buffer CQ overflow — treat `CQOverflow` as critical |

---

## 7. Containers

Verify io_uring is reachable **inside** the container before deploying:

```sh
docker run --rm -v "$PWD:/src" -w /src golang:1.25 \
  sh -c 'CGO_ENABLED=0 go test ./internal/uring/'
```

If that fails with `operation not permitted`, the runtime's seccomp profile is blocking
`io_uring_setup`. Options, least-privilege first: a custom seccomp profile allowing
`io_uring_setup`/`io_uring_enter`/`io_uring_register`, or `--security-opt seccomp=unconfined`
(Kubernetes: a `seccompProfile` of type `Unconfined`, which many clusters disallow by
policy). Recent Docker default profiles block the io_uring syscalls, and several managed
Kubernetes providers disable io_uring at the node level — treat the check above as
authoritative for *your* environment rather than assuming. **If you cannot relax it,
flash-relay cannot run there** — the whole design is io_uring.

---

## 8. Optional: TCP/IP fingerprinting

Only if you need outbound connections to present a macOS/Windows/Android/iOS TCP/IP
fingerprint. Needs `CAP_NET_ADMIN` and the eBPF toolchain:

```sh
sudo apt-get install -y clang llvm libbpf-dev iproute2
cd fingerprint && ./build.sh
sudo tc qdisc add dev eth0 clsact
sudo tc filter add dev eth0 egress bpf da obj bpf/syn_rewrite.bpf.o sec tc
sudo sysctl -w net.core.rmem_max=16777216      # so SO_RCVBUF can reach the target wscales
sudo sysctl -w net.ipv4.tcp_ecn=1              # real ECN for the Apple profiles
sudo bash fingerprint/validate.sh eth0         # asserts all 4 profiles
```

Then call `flashrelay.DialFingerprint(host, port, flashrelay.FPMacOS)` in your Hook.
Details and caveats: [`../fingerprint/README.md`](../fingerprint/README.md). This shapes
only the TCP/IP layer — TLS (JA3/JA4) is the client's and is forwarded untouched.

## 9. Optional: the research rig

Only for reproducing the measurements. Needs **root** (`perf`, `taskset`) plus `python3`,
and it saturates cores — never run it beside production traffic on the same box.

```sh
sudo apt-get install -y linux-tools-common linux-tools-$(uname -r) python3
sudo bash research/gate/harness/gate.sh                      # B1 + B2 vs the netpoll baseline
DRYRUN=1 bash research/gate/harness/saturation-sweep.sh      # placement check, runs nothing
```

See [`../research/README.md`](../research/README.md) and
[`../research/gate/harness/SATURATION-RUN.md`](../research/gate/harness/SATURATION-RUN.md).

---

## 10. Responsible use

flash-relay is proxy/egress infrastructure. The fingerprinting feature shapes only packets
your own host originates, and is intended for legitimate proxy products and authorized
research — deploy it where you control the egress and have authorization to do so.
