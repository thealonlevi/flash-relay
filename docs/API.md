# flash-relay API reference

Package `github.com/thealonlevi/flash-relay/flashrelay`. Linux, `amd64`, pure Go
(`CGO_ENABLED=0`). Also available as godoc: `go doc ./flashrelay`.

## Lifecycle

```go
srv, err := flashrelay.New(cfg, hook) // build + validate
err = srv.Run()                       // blocks: spawns one ring per core, drains on Stop
srv.Stop()                            // async-signal-safe; Run() returns once drained
st := srv.Stat()                      // counter snapshot, any time
```

`New` returns an error for a nil hook or an invalid port. `Run` creates the
`SO_REUSEPORT` listeners (sequentially, to avoid racing the kernel's reuseport-group
setup), starts `Workers` goroutines, and blocks until `Stop`. `Stop` only sets a flag,
so it is safe from a signal handler.

## `Hook` — the decision callback

```go
type Hook func(req []byte, peer netip.AddrPort) Decision
```

Called per connection after the engine reads the initial request bytes (up to
`Config.InitialReqLen`). It runs on an **off-ring goroutine pool**, so it **may block** —
do auth, blacklist lookup, IP allocation, and the upstream dial here. A slow hook parks
one connection; it never stalls the ring.

`req` is valid only for the duration of the call — the engine reuses and may reallocate
the buffer afterwards. Copy anything you retain.

```go
type Decision struct {
    UpstreamFD int    // a connected upstream fd to adopt + relay to (ignored if Reject/More)
    Reply      []byte // sent to the client before relaying / before closing / before reading on
    Reject     bool   // close after sending Reply, without relaying
    More       bool   // request incomplete: read more, then call the Hook again
    Consumed   int    // leading bytes the Hook terminated; NOT forwarded upstream
}
```

Fields are evaluated in strict precedence: **`Reject` → `More` → `UpstreamFD`**. Setting
a lower-precedence field alongside a higher one is a caller bug; the engine ignores it
and closes an ignored `UpstreamFD` rather than leaking it.

Produce `UpstreamFD` with `Dial`/`DialFingerprint` (below) or any connected
`SOCK_STREAM` fd — anything **not** registered with the Go netpoller.

### Multi-segment requests: `More` and `Consumed`

One read is **not** a message boundary — a client request can span TCP segments. Return
`Decision{More: true}` to have the engine read more into the accumulated buffer and
re-invoke the Hook with **everything received so far**. The buffer grows on demand up to
`Config.MaxReqLen`; asking for `More` past that cap closes the connection and counts an
error (it is a memory-amplification vector otherwise).

`Consumed` is how many leading bytes the Hook consumed as protocol handshake — the
engine forwards only `req[Consumed:]` upstream. A SOCKS5 or HTTP CONNECT front end
**must** set it, or it forwards its own handshake into the tunnel. `0` (the default)
forwards everything, which is right for a transparent relay.

```go
// A SOCKS5-shaped hook: multi-round handshake, terminated not forwarded.
hook := func(req []byte, peer netip.AddrPort) flashrelay.Decision {
    if len(req) < 3 {
        return flashrelay.Decision{More: true}
    }
    if !greetingDone(req) {
        // reply, then keep reading — Reply composes with More
        return flashrelay.Decision{Reply: []byte{0x05, 0x00}, More: true}
    }
    n, host, port, ok := parseConnect(req)
    if !ok {
        return flashrelay.Decision{More: true}
    }
    fd, err := flashrelay.Dial(host, port)
    if err != nil {
        return flashrelay.Decision{Reject: true, Reply: socks5Failure}
    }
    return flashrelay.Decision{UpstreamFD: fd, Reply: socks5Success, Consumed: n}
}
```

## `Config`

Zero-value fields take the documented defaults.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `Addr` | string | `0.0.0.0` | bind IP (specific public IP, or `""` = all). IPv4 or IPv6. |
| `Port` | int | — (required) | listen port |
| `Workers` | int | `NumCPU()` | shared-nothing per-core rings (`SO_REUSEPORT`) |
| `Pin` | bool | false | pin worker *i* to CPU `StartCore+i` (one ring/core) |
| `StartCore` | int | 0 | first core to pin to when `Pin` is set |
| `InitialReqLen` | int | 64 | bytes to read before invoking the `Hook` |
| `MaxReqLen` | int | 8192 | cap on the accumulated request buffer when a `Hook` returns `More` |
| `BufSize` | int | 16384 | per-direction relay buffer bytes |
| `MaxConns` | int | 50000 | per-worker live-connection cap (backpressure; shed above) |
| `AcceptBatch` | int | 64 | accepts kept in flight per worker |
| `IdleTimeout` | time.Duration | disabled | close connections idle longer than this |
| `HookWorkers` | int | 256 | off-ring hook goroutines per worker |
| `RingSize` | uint | 4096 | io_uring SQ entries per worker |

## Dialing the upstream

```go
func Dial(host string, port int) (int, error)
```

A **blocking, raw-syscall** TCP connect (IPv4 or IPv6) returning the connected fd. Unlike
`net.Dial` it never registers the fd with the Go netpoller — the whole point. The
blocking connect parks the calling (off-ring hook) goroutine's thread via the Go
scheduler; it does not touch the ring.

```go
func DialFingerprint(host string, port, profile int) (int, error)
```

Like `Dial`, but shapes the SYN to a chosen OS TCP/IP fingerprint: `SO_MARK` selects the
eBPF option-layout/TTL/IP-ID rewrite, `SO_RCVBUF` makes the kernel emit the profile's
window scale, and `IP_TOS` sets the DiffServ/ECN byte. `profile` is one of:

| Constant | Value | OS |
|---|---|---|
| `FPWindows` | 1 | Windows 10/11 |
| `FPMacOS` | 2 | macOS 13–15 |
| `FPAndroid` | 3 | Android 10–14 |
| `FPiOS` | 4 | iOS (iPhone) |

`profile 0` == plain `Dial`. Requires the tc-egress eBPF attached + `CAP_NET_ADMIN`;
full window fidelity needs `net.core.rmem_max` raised and a real NIC. See
[`../fingerprint/`](../fingerprint/) for the eBPF, deploy requirements, and validation.

## `Stats`

`Stat()` returns a snapshot summed across workers:

| Field | Meaning |
|---|---|
| `Accepted` | connections accepted |
| `Completed` | reached the relay phase and closed without landing in another bucket |
| `Rejected` | hook returned `Reject` |
| `Shed` | accepted-then-closed at the backpressure cap |
| `IdleClosed` | closed by the idle timeout |
| `Errors` | accept failures + connections torn down abnormally (incl. peer reset, `MaxReqLen` breach) |
| `BytesC2U` / `BytesU2C` | bytes relayed client→upstream / upstream→client (excludes `Consumed` handshake) |
| `LiveConns` | currently open connections |
| `CQOverflow` | io_uring completions the kernel could not post — expect **0** |

Every accepted connection is counted in **exactly one** terminal bucket, so

```
Accepted == Completed + Rejected + Shed + IdleClosed + <conn errors> + LiveConns
```

holds at rest. (`Errors` also counts accept-syscall failures, which never became
connections and so are not part of that identity.) A nonzero `CQOverflow` is a hard
fault, not a metric: a dropped completion means the op never reports, so its connection
stalls and its fds leak. Modern kernels report `IORING_FEAT_NODROP` and buffer overflow
instead; the engine logs a warning at startup on kernels that do not.

## Notes

- The handler contract (`UpstreamFD` send-then-relay, `Reject` send-then-close) is the
  stable surface; changing it is a breaking change.
- The library is `linux && amd64` only (io_uring + raw syscalls). A complete embedding
  is in [`../examples/echo-relay/`](../examples/echo-relay).
