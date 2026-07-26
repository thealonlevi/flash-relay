#!/usr/bin/env bash
# topology.sh — sourceable CPU-topology helper for the gate harnesses.
#
# WHY THIS EXISTS. On a 2-socket Xeon the kernel commonly numbers logical CPUs
# INTERLEAVED across NUMA nodes, and SMT siblings sit half the range apart. On the
# dual-socket 8176M this repo was prepped for:
#
#   node0 = even CPUs 0,2,...,110      node1 = odd CPUs 1,3,...,111
#   SMT siblings: cpu N pairs with cpu N+56   (0,56) (1,57) (2,58) ...
#
# So the obvious `seq 0 N-1` core list straddles BOTH sockets and doubles up SMT
# siblings. That silently folds the NUMA question (#4) and SMT contention into
# what is supposed to be a clean measurement of reuseport accept-path contention
# (#3) — the two effects become inseparable in the result. Pick cores through
# these helpers instead, and say in the result which placement was used.
#
# Usage:  . "$(dirname "$0")/topology.sh"
#         RELAY=$(take 8 "$(node_primaries 0)")     # 8 physical cores, one socket

# numa_nodes -> "0 1"
numa_nodes() {
  ls -d /sys/devices/system/node/node[0-9]* 2>/dev/null | sed 's/.*node//' | sort -n
}

# expand_list "0-3,8,10-11" -> "0 1 2 3 8 10 11"
expand_list() {
  python3 - "$1" <<'PY'
import sys
out = []
for part in sys.argv[1].split(','):
    part = part.strip()
    if not part:
        continue
    if '-' in part:
        a, b = part.split('-')
        out += list(range(int(a), int(b) + 1))
    else:
        out.append(int(part))
print(' '.join(map(str, out)))
PY
}

# node_cpus <node> -> every logical cpu on that node
node_cpus() {
  local f="/sys/devices/system/node/node$1/cpulist"
  [ -r "$f" ] || return 1
  expand_list "$(cat "$f")"
}

# node_primaries <node> -> one logical cpu per PHYSICAL core on that node
# (SMT siblings dropped, so a run cannot accidentally share a core with itself).
node_primaries() {
  local cpu sibs first seen=" " out=""
  for cpu in $(node_cpus "$1"); do
    sibs=$(cat "/sys/devices/system/cpu/cpu$cpu/topology/thread_siblings_list" 2>/dev/null || echo "$cpu")
    first=${sibs%%,*}; first=${first%%-*}
    case "$seen" in *" $first "*) continue ;; esac
    seen="$seen$first "
    out="$out$first "
  done
  echo "${out% }"
}

# take <n> <space-separated list> -> first n, comma-separated (taskset form)
take() {
  local n=$1; shift
  echo $* | tr ' ' '\n' | head -n "$n" | paste -sd,
}

# drop <n> <space-separated list> -> all but the first n, comma-separated
drop() {
  local n=$1; shift
  echo $* | tr ' ' '\n' | tail -n +$((n + 1)) | paste -sd,
}

# subtract <space-separated pool> <csv or space list to remove> -> space-separated
# remainder. Used to keep load-generation cores strictly disjoint from the SUT's:
# a shared core puts loadgen CPU inside the relay's perf counts and voids the run
# (DESIGN.md §1).
subtract() {
  python3 - "$1" "$2" <<'PY'
import sys
drop = set(sys.argv[2].replace(',', ' ').split())
print(' '.join(c for c in sys.argv[1].split() if c not in drop))
PY
}

# assert_disjoint <label-a> <csv-a> <label-b> <csv-b> — hard-fail on overlap.
assert_disjoint() {
  local common
  common=$(python3 - "$2" "$4" <<'PY'
import sys
a = set(sys.argv[1].replace(',', ' ').split())
b = set(sys.argv[2].replace(',', ' ').split())
print(','.join(sorted(a & b, key=int)))
PY
)
  if [ -n "$common" ]; then
    echo "FATAL: $1 and $3 share cores [$common] — loadgen CPU would pollute the SUT's perf counts" >&2
    return 1
  fi
  return 0
}

# topology_summary -> one human line per node, for the result header
topology_summary() {
  local n
  echo "sockets/NUMA nodes: $(numa_nodes | tr '\n' ' ')"
  for n in $(numa_nodes); do
    local prim; prim=$(node_primaries "$n")
    echo "  node$n: $(echo "$prim" | wc -w) physical cores  [$(echo "$prim" | tr ' ' ',' | cut -c1-60)...]"
  done
  echo "  SMT active: $(cat /sys/devices/system/cpu/smt/active 2>/dev/null || echo '?')"
}
