# MONITOR.md — optimizer supervision playbook (I update this each check-in)

The optimizer runs **detached** (`research/optimizer/loop.sh`, pidfile `research/optimizer/results/loop.pid`)
on branch `optimizer-run`, Opus, N=5. It does NOT need me between check-ins — I'm
the safety net. **Keep the box quiet** during check-ins: the loop's `score.sh`
pins the relay to core 6 (+ sink 7, loadgen 9-12) and measures instr/conn — do
NOT run builds, benchmarks, or anything heavy on those cores, and do NOT touch
`research/gate/` files (the optimizer is mutating them on its branch).

## Each 30-min check-in — do these (all light / read-only unless acting):
1. **Alive?** `PID=$(cat research/optimizer/results/loop.pid); kill -0 $PID`.
   - If dead: read the tail of `research/optimizer/results/loop-logs/loop.out` + `claude-stderr.log`
     to find why, then **restart**: `bash research/optimizer/start.sh`. Log the cause here.
2. **Progressing?** `tail -15 research/optimizer/results/iterations.csv`. Look at:
   - champion column trending up (promotions landing),
   - verdict mix — a long run of `revert-fail` with the same `reason`
     (build_fail / ring_test_fail / two_fd_fail / duplex_broken) means the agent
     is stuck breaking things → not fatal (auto-reverts), but note the pattern.
   - `no_improve` near PATIENCE(50) → it will self-stop at plateau (expected end).
3. **Errors?** `tail -20 research/optimizer/results/loop-logs/claude-stderr.log` — watch for
   auth/rate-limit/timeouts. Occasional is fine (loop skips + continues).
4. **ClickHouse sane?** `clickhouse-client --query "SELECT count(), max(champion) FROM flashrelay.iterations"`.
5. **Cost** (tracking only, budget unconstrained): last `cum_cost` in iterations.csv.
6. **Update this file** with what I observed + what to watch next, and reschedule.

## Dead vs. plateau-finished (IMPORTANT — don't blindly restart)
If the pidfile check shows DEAD, look at `tail research/optimizer/results/loop-logs/loop.out`
FIRST:
- ends with `plateau (... w/o improvement) -> stop` or `ended at iter N` →
  it FINISHED CLEANLY (hit PATIENCE). Do NOT restart (it would just re-plateau).
  Instead: champion is final (research/optimizer/results/BEST.json); write a closing
  summary line here and leave it stopped.
- ends mid-iteration / a crash / nothing → genuine death → restart with
  `bash research/optimizer/start.sh`.

## Stuck-champion check (learned 2026-06-20)
If the champion hasn't moved for many iters AND scores cluster *uniformly* ~X%
below it (not a spread of near-misses), suspect a **stale-config baseline** — the
champion was scored under a different N/MEASURE than the loop now uses, so the bar
is unbeatable. Fix: graceful STOP → `bash research/optimizer/score.sh` on HEAD (quiet box)
→ write the true score into research/optimizer/results/BEST.json (keep current config!) →
restart. Do NOT change N/MEASURE after baselining or you reintroduce the mismatch.

## When to STOP it (`touch research/optimizer/results/STOP` — graceful after current iter):
- Plateau reached (it self-stops; just confirm + write a summary).
- Pathological: process won't stay up across 2 restarts, or every iteration fails
  with the same reason and champion hasn't moved in many iterations.
- Morning / user asks.

## Do NOT:
- Run `git` on optimizer-run (the loop owns it). Read-only git elsewhere is fine.
- Run heavy loads / builds / B3 while the loop is measuring (perturbs instr/conn).
- Edit `research/gate/internal/uring` or `research/gate/cmd/relay-uring` (the optimizer's territory).

## FINAL — run complete (2026-06-21 00:32)
Optimizer **plateau-stopped cleanly** at iter 50 (no_improve=50). NOT restarting
(clean finish, not a crash). Cron c6d35d9a CANCELLED (supervised task done).
- **Champion: multishot accept** (commit ab3a110 on `optimizer-run`). Net change
  vs main: +22/-2 across research/gate/internal/uring + research/gate/cmd/relay-uring. Builds clean,
  ring tests pass, duplex intact. The loop's own A/B: baseline 137913 → 133431
  instr/conn (~3% fewer) — a genuine win it found on iteration 1 and held.
- **64 iterations, ~$91, zero errors, anti-cheat held throughout** (incl. the agent
  probing score.sh — no gaming, no anomalous scores). Everything else explored
  (buffer pooling/coalescing, submit-harvest restructuring, map removal, registered
  files attempts) stayed within this VM's ~4% measurement noise → not promotable.
- Mid-run fix: re-baselined a stale-config champion (7494.5 was an N=3 score vs the
  N=5 loop) → unstuck the climb. See check-in #2.
- **Verdict:** multishot is the achievable headline win on this noisy KVM. Sub-1%
  micro-wins exist but need a lower-noise (bare-metal) box + lower EPSILON to
  confirm/accumulate.

## Morning handoff:
- Champion: `research/optimizer/results/BEST.json` (+ commit hash on `optimizer-run`).
- To adopt wins: review the `optimizer-run` diff vs `main`, then merge/cherry-pick
  the champion into `main`. The pidfix + Opus-config commits also live on
  optimizer-run and should land on main.

## Check-in log
- 2026-06-20 21:11 — launched Opus/N=5. (Restarted twice for fixes: pidfile
  check, gate-scoped change-detection. Final pid 218313.)
- 2026-06-20 21:21 — check-in #1. RUNNING (pid 218313), iter 3. Champion 7494.5
  (multishot accept). iters: 1 promote, then 2 revert-regression (6922.7, 6908.3
  — both correctly reverted, reason=ok i.e. correctness fine, just slower). No
  claude errors. ClickHouse logging OK (3 rows). Hill-climb healthy. Cadence cron
  c6d35d9a (:13/:43). WATCH NEXT: whether Opus finds gains past multishot or the
  regression streak grows toward PATIENCE(50) plateau; cum_cost trend.
- 2026-06-20 22:00 — check-in #2 (cron). RUNNING, but champion was STUCK at
  7494.5 for 12 iters, all mutations clustered ~6800-7000. Diagnosed: stale-config
  baseline — 7494.5 was an N=3 score; re-scored champion (multishot, HEAD ab3a110)
  under current N=5 = **6989.5** (iter 8's 6993.8 had actually been a real micro-win
  wrongly reverted). STOP→re-baselined BEST.json to 6989.5→restarted (pid 230122,
  threshold now 7199). $10.12 spent over the 13 stuck iters. WATCH NEXT: real
  promotions should now appear above 7199; confirm no_improve resets and climbs.
- 2026-06-20 22:21 — check-in #3 (cron). RUNNING (pid 230122), iter 6,
  no_improve=5. Re-baseline WORKING: scores now cluster around the true 6989.5
  (7096.5, 6895, 6818.7, 6919.9, 6919) instead of uniformly 7% below. iter1's
  7096.5 was a real ~1.5% gain but below the 3% promote threshold (7199) →
  reverted (correct: it's within the ~7% per-rep noise). No errors. $15.9 total.
  ASSESSMENT: healthy exploration, not pathological. The limiter is now
  MEASUREMENT NOISE — ~7% per-rep spread → ~3% mean-noise ≈ EPSILON, so only
  ≥3-4% real wins can promote. multishot captured the big accept-path win;
  finer gains sit under this noisy KVM's floor (a quieter/bare-metal box would
  resolve more). May plateau with multishot as the headline win — a legitimate
  result. WATCH NEXT: any ≥3% promotion, or no_improve→50 plateau (self-stops).
  Not changing EPSILON/N (sub-noise tuning would just promote noise).
- 2026-06-20 22:55 — check-in #4 (cron). RUNNING (pid 230122), iter ~10,
  no_improve=9. Still no promotion: 9 post-rebaseline scores all 6818-7096,
  champion holds 6989.5. No errors. COST climbing: $31 total, per-iter $0.75→$4.69
  (resumed Opus session context growing + heavier exploration; will rotate at
  120k tok). Read: legit hill-climb, multishot likely a strong local optimum;
  finer io_uring wins (registered files / batched submit / MSG_MORE) probably sit
  under the ~4% across-iter measurement scatter. Held off tuning N (VM noise is
  correlated → more reps wouldn't reliably help; avoid thrashing). WATCH NEXT:
  no_improve→50 plateau (self-stops, legit result = multishot headline); cost
  trend; whether any ≥3% win lands. Morning: a quieter/bare-metal box would let
  it resolve sub-4% wins the optimizer can't confirm here.
- 2026-06-20 23:23 — check-in #5 (cron). RUNNING (pid 230122), iter 19,
  no_improve=18/50. Still no promotion (18 straight, scores 6801-7096, champion
  6989.5). No errors. $45 total, per-iter settled ~$1-2.7. Plateau approaching —
  letting it run to PATIENCE=50 self-stop (budget unconstrained; multishot looks
  like the achievable optimum here). ANTI-CHEAT WATCH: agent is now reading
  research/optimizer/score.sh (expected adversarial probe) — gate holds (can't edit scorer;
  two-fd/byte-audit/duplex/locked-paths intact), NO anomalous score spike (max
  7096 < 7199). WATCH NEXT: plateau self-stop; any sudden gate-passing score jump
  (gaming signal → inspect the promoted diff); cost.
- 2026-06-20 23:53 — check-in #6 (cron). RUNNING (pid 230122), iter 30,
  no_improve=29/50. Stable plateau: champion 6989.5; recent mutations now land
  JUST above baseline (7029/7022/7010/7000) — real ~0.5-1% micro-wins over
  multishot but under the +3% bar, so reverted (noise floor; greedy climb can't
  stack sub-threshold gains). No errors. $61.65, per-iter ~$1.3-2.2. Will
  self-stop at no_improve=50 (~21 iters, ~35min). Letting it finish (go-wild
  mandate). NEXT CHECK-IN: it may show DEAD due to clean plateau-stop — see the
  "Dead vs plateau-finished" note above; do NOT restart if loop.out ends with
  'plateau/ended', just write the closing summary. Morning takeaway: multishot is
  the headline win on this box; sub-1% micro-wins exist but need a quieter
  (lower-noise) box + lower EPSILON to capture/accumulate.
- 2026-06-21 00:24 — check-in #7 (cron). RUNNING (pid 230122), iter 46,
  no_improve=45/50 — ~5 iters from plateau self-stop (~8 min). Champion FINAL
  6989.5 (multishot accept). Scores still 6950-7054, nothing clears +3%. No
  errors. $85.52. RESULT SETTLED: multishot is the run's headline win; all other
  io_uring ideas explored stayed under the noise floor. NEXT CHECK-IN: expect
  DEAD via clean plateau-stop → confirm loop.out ends 'plateau...stop', do NOT
  restart, write the closing summary (champion = research/optimizer/results/BEST.json,
  hash ab3a110 on optimizer-run; morning: merge that into main).
