#!/usr/bin/env bash
# nic-steer.sh — put the NIC's RX processing on the SUT's cores, reversibly.
#
# WHY. A scaling sweep asks "does core N+1 buy more conn/s". On a real NIC that
# question is only well posed if the NIC scales WITH the relay. Two failure modes
# otherwise, both of which look like the relay failing to scale:
#
#   * RSS spreads RX across every core on the box (this box ships 80 combined
#     queues). At N=1 the single relay core does the relay work while ~39 other
#     cores do its packet processing — the point measures a 1-core relay backed by
#     40 cores of softirq, and `perf -p <relay>` counts NONE of that softirq. The
#     result flatters the low-N points and undercounts instructions/conn.
#   * Conversely, too FEW queues serializes RX and caps conn/s at a number that has
#     nothing to do with the relay.
#
# So each sweep point should get N relay cores AND N NIC queues, IRQ-pinned to
# exactly those cores. Then "cores used" means what it says.
#
# irqbalance MUST be stopped first: it periodically rewrites smp_affinity and will
# silently undo this, which is worse than not steering at all because the run looks
# steered. This script stops it and restores it on `restore`.
#
# Usage:
#   sudo bash nic-steer.sh steer   eno1np0 0,1,2,3     # 4 queues on cores 0-3
#   sudo bash nic-steer.sh show    eno1np0
#   sudo bash nic-steer.sh restore eno1np0             # undo, restart irqbalance
#
# State is saved to /var/tmp/nic-steer-<iface>.state so restore works across shells.
set -uo pipefail

IFACE=${2:?usage: nic-steer.sh <steer|show|restore> <iface> [cores-csv]}
STATE="/var/tmp/nic-steer-$IFACE.state"

irq_list() { grep "$IFACE" /proc/interrupts | awk -F: '{gsub(/ /,"",$1); print $1}'; }

cmd_show() {
  echo "iface: $IFACE"
  echo "combined queues: $(ethtool -l "$IFACE" 2>/dev/null | awk '/Current hardware/,0' | awk '/Combined/{print $2}')"
  echo "irqbalance: $(systemctl is-active irqbalance 2>/dev/null)"
  echo "IRQ -> affinity:"
  local n=0
  for irq in $(irq_list); do
    printf "  irq %-5s cpus %s\n" "$irq" "$(cat "/proc/irq/$irq/smp_affinity_list" 2>/dev/null)"
    n=$((n + 1)); [ "$n" -ge 12 ] && { echo "  ... ($(irq_list | wc -l) total)"; break; }
  done
}

cmd_steer() {
  local cores=${3:?usage: nic-steer.sh steer <iface> <cores-csv>}
  [ "$(id -u)" = 0 ] || { echo "run as root"; exit 1; }
  local list; list=$(echo "$cores" | tr ',' ' ')
  local n; n=$(echo "$list" | wc -w)

  if [ ! -f "$STATE" ]; then
    {
      echo "IRQBALANCE_WAS=$(systemctl is-active irqbalance 2>/dev/null)"
      echo "COMBINED_WAS=$(ethtool -l "$IFACE" 2>/dev/null | awk '/Current hardware/,0' | awk '/Combined/{print $2}')"
      for irq in $(irq_list); do
        echo "IRQ_$irq=$(cat "/proc/irq/$irq/smp_affinity_list" 2>/dev/null)"
      done
    } > "$STATE"
    echo "saved original state -> $STATE"
  fi

  # irqbalance rewrites affinity on its own schedule; stop it or this is theatre.
  if [ "$(systemctl is-active irqbalance 2>/dev/null)" = active ]; then
    systemctl stop irqbalance && echo "stopped irqbalance (restored by: $0 restore $IFACE)"
  fi

  echo "setting $n combined queues on $IFACE"
  ethtool -L "$IFACE" combined "$n" 2>&1 | sed 's/^/  /' || \
    echo "  !! ethtool -L failed — queue count unchanged; steering will be partial"
  sleep 1

  local i=0
  for irq in $(irq_list); do
    local cpu; cpu=$(echo "$list" | tr ' ' '\n' | sed -n "$(( i % n + 1 ))p")
    echo "$cpu" > "/proc/irq/$irq/smp_affinity_list" 2>/dev/null \
      && printf "  irq %-5s -> cpu %s\n" "$irq" "$cpu" \
      || printf "  irq %-5s -> cpu %s FAILED (managed IRQ?)\n" "$irq" "$cpu"
    i=$((i + 1))
  done
  echo "done. verify with: $0 show $IFACE"
}

cmd_restore() {
  [ "$(id -u)" = 0 ] || { echo "run as root"; exit 1; }
  [ -f "$STATE" ] || { echo "no saved state at $STATE — nothing to restore"; exit 1; }
  # shellcheck disable=SC1090
  local combined was
  combined=$(grep '^COMBINED_WAS=' "$STATE" | cut -d= -f2)
  was=$(grep '^IRQBALANCE_WAS=' "$STATE" | cut -d= -f2)
  [ -n "$combined" ] && { echo "restoring $combined combined queues"; ethtool -L "$IFACE" combined "$combined" 2>&1 | sed 's/^/  /'; sleep 1; }
  while IFS='=' read -r k v; do
    case "$k" in
      IRQ_*) echo "$v" > "/proc/irq/${k#IRQ_}/smp_affinity_list" 2>/dev/null || true ;;
    esac
  done < "$STATE"
  if [ "$was" = active ]; then systemctl start irqbalance && echo "restarted irqbalance"; fi
  rm -f "$STATE"
  echo "restored."
}

case "${1:-}" in
  steer)   cmd_steer "$@" ;;
  show)    cmd_show ;;
  restore) cmd_restore ;;
  *) echo "usage: $0 <steer|show|restore> <iface> [cores-csv]"; exit 1 ;;
esac
