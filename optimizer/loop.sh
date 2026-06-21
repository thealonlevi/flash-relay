#!/usr/bin/env bash
# optimizer/loop.sh — the autonomous hill-climb. One mutation per iteration:
#   MUTATE:  `claude -p` makes ONE focused edit to the io_uring relay hot path.
#   MEASURE: optimizer/score.sh measures the objective(s) + per-arm anti-cheat gate.
#   VERDICT: keep or revert.
#
# MODE=pareto (default): measure BOTH objectives (instr_pc AND bytes_per_cpu) and
# keep a mutation ONLY if it improves at least one by EPSILON and regresses NEITHER
# beyond REGRESS_TOL — so both objectives ratchet up and neither is sacrificed for
# the other. Champion-promoting candidates additionally clear the safety gate suite
# (flood-survive + RSS-slope; the cross-objective gate is redundant here). MODE=single
# maximizes OBJECTIVE with the full gate suite protecting everything else.
# Runs on a dedicated branch so main stays clean. Stop: touch optimizer/results/STOP
set -uo pipefail
cd "$(dirname "$0")/.."
. optimizer/config
export IS_SANDBOX=1 PATH="$PATH:/usr/local/go/bin" CGO_ENABLED=0
mkdir -p "$RESULTS_DIR/loop-logs"
echo $$ > "$RESULTS_DIR/loop.pid"
trap 'rm -f "$RESULTS_DIR/loop.pid"' EXIT
LOG(){ echo "[loop $(date +%H:%M:%S)] $*"; }
MAX_ITERS="${MAX_ITERS:-1000000}"
ITERCSV="$RESULTS_DIR/iterations.csv"
[ -f "$ITERCSV" ] || echo "iter,utc,instr_pc,bytes_per_cpu,champ_instr,champ_bytes,verdict,reason,cost_usd,cum_cost,in_tokens" > "$ITERCSV"

git rev-parse --verify "$OPT_BRANCH" >/dev/null 2>&1 || git branch "$OPT_BRANCH"
git checkout -q "$OPT_BRANCH" || { LOG "cannot checkout $OPT_BRANCH"; exit 1; }

PORT_A=40000; PORT_B=44000   # separate verified-bind port bases per arm
wt_revert(){ git checkout -- . 2>/dev/null; git clean -fdq $ALLOWED_PATHS 2>/dev/null; }  # pre-commit (no reset!)
getf(){ python3 -c "import sys,json;print(json.load(sys.stdin).get('$1',0))" 2>/dev/null || echo 0; }
champf(){ python3 -c "import json;print(json.load(open('$BEST')).get('$1',0))" 2>/dev/null || echo 0; }
# measure one objective; echoes "<score> <reason>"
measure(){ local r; r=$(OBJECTIVE="$1" PORT_BASE="$2" bash optimizer/score.sh 2>>"$RESULTS_DIR/loop-logs/score.log"); echo "$(echo "$r"|getf score) $(echo "$r"|getf reason)"; }

# --- baseline champion ------------------------------------------------------
if [ ! -f "$BEST" ]; then
  LOG "no champion -> measuring baseline (both objectives)"
  read -r a ar <<<"$(measure instr_pc $PORT_A)"
  read -r b br <<<"$(measure bytes_per_cpu $PORT_B)"
  python3 -c "import json;json.dump({'instr_pc':float('$a'),'bytes_per_cpu':float('$b'),'note':'baseline'},open('$BEST','w'))"
  LOG "baseline instr_pc=$a ($ar) bytes_per_cpu=$b ($br)"
fi

SID=""; iter=0; no_improve=0; CUM=0
LOG "start. mode=$MODE champion: instr_pc=$(champf instr_pc) bytes_per_cpu=$(champf bytes_per_cpu) model='${OPT_MODEL:-default}' branch=$OPT_BRANCH"

while [ "$iter" -lt "$MAX_ITERS" ]; do
  [ -f "$RESULTS_DIR/STOP" ] && { LOG "STOP -> exit"; rm -f "$RESULTS_DIR/STOP"; break; }
  iter=$((iter+1)); LOG "==== iter $iter (no_improve=$no_improve/$PATIENCE) ===="
  tail -8 "$ITERCSV" > "$RESULTS_DIR/RECENT.csv" 2>/dev/null || true

  out=$(timeout "$ITER_TIMEOUT" env IS_SANDBOX=1 claude -p "$(cat optimizer/optimizer-prompt.md)" \
        ${SID:+--resume "$SID"} ${OPT_MODEL:+--model "$OPT_MODEL"} \
        --append-system-prompt-file optimizer/optimizer-system.md \
        --dangerously-skip-permissions --output-format json 2>>"$RESULTS_DIR/loop-logs/claude-stderr.log")
  rc=$?
  if [ "$rc" -ne 0 ]; then LOG "claude rc=$rc -> revert wt, skip"; wt_revert; no_improve=$((no_improve+1)); continue; fi
  HYP=$(echo "$out" | python3 -c "import sys,json;print(json.load(sys.stdin).get('result','')[:300])" 2>/dev/null)
  SID=$(echo "$out" | python3 -c "import sys,json;print(json.load(sys.stdin).get('session_id',''))" 2>/dev/null)
  intok=$(echo "$out" | python3 -c "import sys,json;print(json.load(sys.stdin).get('usage',{}).get('input_tokens',0))" 2>/dev/null || echo 0)
  cost=$(echo "$out" | python3 -c "import sys,json;print(json.load(sys.stdin).get('total_cost_usd',0))" 2>/dev/null || echo 0)
  LOG "claude in_tok=$intok cost=\$$cost hyp='${HYP:0:70}'"
  [ "${intok:-0}" -gt "$CTX_ROTATE_TOKENS" ] && { LOG "ctx>$CTX_ROTATE_TOKENS -> rotate session"; SID=""; }

  # enforce the LOCKED contract: only ALLOWED_PATHS changes are considered/committed.
  changed=$(git status --porcelain -- $ALLOWED_PATHS | awk '{print $2}')
  [ -z "$changed" ] && { LOG "no allowed-path changes -> skip measure"; continue; }
  viol=0
  for f in $changed; do ok=0; for p in $ALLOWED_PATHS; do case "$f" in $p/*) ok=1;; esac; done; [ "$ok" = 0 ] && { LOG "VIOLATION edit outside allowed: $f"; viol=1; }; done
  [ "$viol" = 1 ] && { wt_revert; no_improve=$((no_improve+1)); continue; }

  git add -A -- $ALLOWED_PATHS
  git commit -q -m "optimize(iter $iter): ${HYP:-mutation}" --author="optimizer <opt@flash-relay.local>"

  # --- measure both objectives ---------------------------------------------
  read -r a ar <<<"$(measure instr_pc $PORT_A)"
  read -r b br <<<"$(measure bytes_per_cpu $PORT_B)"
  ca=$(champf instr_pc); cb=$(champf bytes_per_cpu)
  LOG "measured instr_pc=$a ($ar) bytes_per_cpu=$b ($br) | champ instr_pc=$ca bytes_per_cpu=$cb"
  reason="ok"; [ "$ar" != ok ] && reason="instr:$ar"; [ "$br" != ok ] && reason="bytes:$br"

  # Pareto decision: keep iff regresses neither (within TOL) and improves >=1 (by EPS).
  decision=$(A=$a B=$b CA=$ca CB=$cb EPS=$EPSILON TOL=$REGRESS_TOL python3 - <<'PY'
import os
a,b=float(os.environ['A']),float(os.environ['B'])
ca,cb=float(os.environ['CA']),float(os.environ['CB'])
eps,tol=float(os.environ['EPS']),float(os.environ['TOL'])
if a<=0 or b<=0: print('fail')
elif not (a>=ca*(1-tol) and b>=cb*(1-tol)): print('regress')
elif (a>ca*(1+eps)) or (b>cb*(1+eps)): print('promote')
else: print('noimprove')
PY
)

  case "$decision" in
    fail)     VERDICT="revert-fail"; git reset --hard HEAD~1 -q; no_improve=$((no_improve+1));;
    regress)  VERDICT="revert-regression"; reason="${reason/ok/}regress"; git reset --hard HEAD~1 -q; no_improve=$((no_improve+1));;
    noimprove) VERDICT="revert-noimprove"; git reset --hard HEAD~1 -q; no_improve=$((no_improve+1));;
    promote)
      # Pareto-improving on the objectives. Now clear the SAFETY gate suite
      # (flood-survive + RSS-slope). No CROSS_BASELINE -> the redundant
      # cross-objective gate is skipped (Pareto already enforces no-regression).
      gpass=1; greason="suite_off"
      if [ "${RUN_GATE_SUITE:-1}" = 1 ]; then
        gs=$(bash optimizer/gate-suite.sh 2>>"$RESULTS_DIR/loop-logs/gate-suite.log")
        gpass=$(echo "$gs" | getf pass); greason=$(echo "$gs" | getf reason)
      fi
      if [ "$gpass" != 1 ]; then
        VERDICT="revert-gatefail"; reason="gate_$greason"; git reset --hard HEAD~1 -q; no_improve=$((no_improve+1))
        LOG "Pareto win REJECTED by safety gate ($greason) -> revert"
      else
        VERDICT="promote"; no_improve=0
        python3 -c "import json;json.dump({'instr_pc':float('$a'),'bytes_per_cpu':float('$b'),'hash':'$(git rev-parse HEAD)','note':'champion'},open('$BEST','w'))"
        LOG "*** NEW CHAMPION instr_pc=$a bytes_per_cpu=$b (was $ca/$cb); gates ok ***"
      fi;;
  esac

  CUM=$(awk -v a="$CUM" -v b="${cost:-0}" 'BEGIN{printf "%.4f",a+b}')
  utc=$(date -u +%FT%TZ)
  echo "$iter,$utc,$a,$b,$ca,$cb,$VERDICT,$reason,${cost:-0},$CUM,${intok:-0}" >> "$ITERCSV"
  echo "{\"ts\":\"$(date -u +'%F %T')\",\"iter\":$iter,\"score\":$a,\"score_bytes\":$b,\"champion\":$ca,\"champion_bytes\":$cb,\"verdict\":\"$VERDICT\",\"reason\":\"$reason\",\"cost_usd\":${cost:-0},\"cum_cost_usd\":$CUM,\"in_tokens\":${intok:-0},\"hypothesis\":$(python3 -c "import json,sys;print(json.dumps(sys.argv[1]))" "$HYP" 2>/dev/null||echo '""')}" \
    | clickhouse-client --query "INSERT INTO $CH_DB.iterations FORMAT JSONEachRow" 2>/dev/null || true

  [ "$no_improve" -ge "$PATIENCE" ] && { LOG "plateau ($no_improve w/o improvement) -> stop"; break; }
done
LOG "ended at iter $iter. champion: instr_pc=$(champf instr_pc) bytes_per_cpu=$(champf bytes_per_cpu)."
