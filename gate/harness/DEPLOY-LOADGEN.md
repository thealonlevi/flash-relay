# Deploying the loadgen box (2-box measurement-grade gate)

This is the **loadgen box** setup for a measurement-grade B1/B2 run. The SUT (the
relay) runs on box 1; this box (box 2) runs **both** the connection-storm client
(`loadgen`) **and** the upstream `sink`, so *both* relay legs (client→relay and
relay→upstream) go over a real NIC instead of loopback.

```
   ┌─────────────── box 1 (SUT) ───────────────┐        ┌──────── box 2 (loadgen) ────────┐
   │  relay-uring / relay-netpoll               │        │  loadgen  (the storm client)     │
   │  pinned to 1 core, measured with perf      │        │  sink     (the upstream)         │
   │  listens on  BOX1_IP:18000                 │◄──NIC──┤  loadgen dials BOX1_IP:18000     │
   │  dials upstream  BOX2_IP:9100              ├───NIC─►│  sink listens BOX2_IP:9100       │
   └────────────────────────────────────────────┘        └──────────────────────────────────┘
```

Set these for your network:

```bash
BOX1_IP=10.0.0.1     # SUT box (relay)
BOX2_IP=10.0.0.2     # this box (loadgen + sink)
RPORT=18000          # relay listen port      (open inbound on box 1)
SPORT=9100           # sink  listen port      (open inbound on box 2)
```

## 1. Requirements

- Linux x86-64, kernel ≥ 5.x. **≥ 4 cores** (loadgen is multi-core; it must
  out-drive the single SUT core). Root (for sysctl + ulimit).
- Network reachability: box 2 → `BOX1_IP:RPORT`, and box 1 → `BOX2_IP:SPORT`.

## 2. Get the binaries

`sink` and `loadgen` are pure Go (`CGO_ENABLED=0`), so build once and copy.

**Option A — build on box 1 and copy (no Go needed on box 2):**
```bash
# on box 1, in the repo:
for c in sink loadgen loadgend; do
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/$c ./gate/cmd/$c
done
scp /tmp/sink /tmp/loadgen /tmp/loadgend  user@$BOX2_IP:/usr/local/bin/
```

**Option B — build on box 2 (needs Go ≥ 1.25):**
```bash
git clone git@github.com:thealonlevi/flash-relay.git && cd flash-relay
for c in sink loadgen loadgend; do CGO_ENABLED=0 go build -o /usr/local/bin/$c ./gate/cmd/$c; done
```

`loadgend` is the **control daemon**: it hosts the sink in-process and exposes an
HTTP endpoint so box 1 drives the whole run remotely (recommended — §5a). `sink`
and `loadgen` remain for the manual flow (§5b).

## 3. Kernel + ulimit tuning (REQUIRED — connection storm)

A churn client opens/closes huge numbers of short TCP connections. Without
tuning you hit ephemeral-port exhaustion, TIME_WAIT pileup, fd limits, or
conntrack overflow — all of which throttle the client and **understate** the
relay. Apply on **both** boxes (the relay also dials upstream per connection):

```bash
sudo tee /etc/sysctl.d/99-flashrelay-bench.conf >/dev/null <<'EOF'
net.ipv4.ip_local_port_range = 1024 65535
# IMPORTANT: reserve the relay + sink LISTEN ports so the (now wide) ephemeral
# source-port allocator never grabs them. Without this, under high outbound
# churn the kernel can assign 18000/9100 as an ephemeral source port, and the
# next listener bind fails with EADDRINUSE (a self-collision). Reserve every
# fixed port you listen on / dial to as a fixed dest.
net.ipv4.ip_local_reserved_ports = 9100,18000
net.ipv4.tcp_tw_reuse = 1
net.ipv4.tcp_fin_timeout = 10
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 65535
net.core.netdev_max_backlog = 250000
net.ipv4.tcp_max_tw_buckets = 2000000
EOF
sudo sysctl --system

# file descriptors (per shell that runs loadgen/sink)
ulimit -n 1048576
```

If `nf_conntrack` is loaded (check `lsmod | grep conntrack`), either raise it
(`sudo sysctl -w net.netfilter.nf_conntrack_max=2000000`) or, on a dedicated
bench box, unload it. Conntrack overflow silently drops connections.

## 4. Firewall

```bash
# box 2 (this box): allow the relay to reach the sink, and box 1 to reach the
# loadgend control port (9200).
sudo iptables -I INPUT -p tcp --dport $SPORT -s $BOX1_IP -j ACCEPT
sudo iptables -I INPUT -p tcp --dport 9200  -s $BOX1_IP -j ACCEPT
# box 1: allow box 2 to reach the relay  (run-2box.sh adds this automatically)
# sudo iptables -I INPUT -p tcp --dport $RPORT -s $BOX2_IP -j ACCEPT
```

## 5a. Run it — daemon-driven (recommended, one command from box 1)

Start the control daemon **once** on box 2 (it hosts the sink in-process and
serves the loadgen on demand); leave it running:
```bash
ulimit -n 1048576
loadgend -control 0.0.0.0:9200 -sink 0.0.0.0:$SPORT -relay $BOX1_IP:$RPORT \
  -reqlen 64 -replylen 256
```

Then drive the **entire** 2-box run from box 1 — both builds, perf, latency,
verdict — with a single command:
```bash
sudo env B2=$BOX2_IP BOX1_IP=$BOX1_IP bash gate/harness/run-2box.sh
# realistic-dial parking test:
sudo env B2=$BOX2_IP BOX1_IP=$BOX1_IP REALISTIC=1 INFLIGHT=8000 bash gate/harness/run-2box.sh
```
`run-2box.sh` reserves the listen port, adds the box-1 inbound firewall rule,
checks the daemon's `/health`, then for each build fires the remote storm
(`curl http://$BOX2_IP:9200/run?...`), runs the SUT harness, collects the JSON,
and writes `results/2box-<ts>/SUMMARY.md`. Knobs (env): `DUR REPS INFLIGHT WARMUP
STORM_DUR CORE_SUT AUTHCPU REALISTIC RPORT SPORT CONTROL`.

The daemon also responds to a direct probe if you want to sanity-check it:
```bash
curl -s "http://$BOX2_IP:9200/run?inflight=64&warmup=2s&duration=4s"   # returns JSON
```

## 5b. Run it — manual (no daemon)

**Step A — start the sink on box 2 (leave it running):**
```bash
ulimit -n 1048576
sink -addr $BOX2_IP:$SPORT -reqlen 64 -replylen 256
```

**Step B — measure each build.** Do the io_uring SUT first, then the baseline.
For each:

1. On **box 1**, start the SUT-side harness (it starts the relay + runs perf and
   waits for load):
   ```bash
   # NOTE: `sudo env VAR=…` — plain `sudo VAR=… bash` drops the vars (sudo
   # sanitizes the environment).
   sudo env BUILD=uring   SINK=$BOX2_IP:$SPORT RPORT=$RPORT DUR=10 REPS=5 \
     bash gate/harness/run-sut.sh
   # later, second pass:
   sudo env BUILD=netpoll SINK=$BOX2_IP:$SPORT RPORT=$RPORT DUR=10 REPS=5 \
     bash gate/harness/run-sut.sh
   ```
2. When box 1 prints `START THE LOADGEN`, run this on **box 2** and save the JSON
   (it must outlast box 1's measurement window — REPS×DUR + ramp; e.g. ≥ 70s):
   ```bash
   ulimit -n 1048576
   loadgen -relay $BOX1_IP:$RPORT -inflight 512 -warmup 5s -duration 65s \
     -reqlen 64 -replylen 256 > uring_loadgen.json      # name per build
   ```
3. Back on box 1, press Enter (or let the ramp elapse); it measures and writes
   `results/2box-<ts>/`.

**Realistic-dial variant** (parking/concurrency test, DESIGN §4): add
`REALISTIC=1` on box 1 and raise `-inflight` on box 2 (e.g. 8000) so the core
stays busy despite the ms-scale dial parks.

## 6. Combine into a verdict

Copy the two loadgen JSONs to box 1, then:
```bash
python3 gate/harness/combine-2box.py results/2box-<ts> \
  uring_loadgen.json netpoll_loadgen.json
```
This writes `SUMMARY.md`: B1 (fd-registration symbols — must be 0 for the SUT),
B2 (conn/s-per-core + instructions/conn ratio), and the latency tuple.

## 7. Scaling notes (for later, B3/B4)

- **B3 (50–100k concurrent):** one client→`BOX1_IP:RPORT` 4-tuple caps near ~64k
  source ports. To go higher, add relay ports (SO_REUSEPORT) and spread loadgen
  across them, and/or give box 2 multiple source IPs.
- **B4 (throughput):** needs a 10GbE+ link and large `-reqlen/-replylen`; put the
  sink on a third box so the pump is end-to-end.
