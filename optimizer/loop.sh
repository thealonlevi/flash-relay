#!/usr/bin/env bash
# optimizer/loop.sh — the autonomous hill-climb. Replicates flashaccept's
# research/harness/loop.sh, tuned for the relay. One mutation per iteration:
#   ANALYTICS: `claude -p` makes ONE focused edit to the io_uring relay hot path.
#   MEASURE:   optimizer/score.sh builds/pins/measures instr/conn + anti-cheat gate.
#   VERDICT:   promote (commit stays, update BEST.json) if >EPSILON better, else
#              revert (git reset). Edits outside ALLOWED_PATHS are reverted wholesale.
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
[ -f "$ITERCSV" ] || echo "iter,utc,score,champion,verdict,reason,cost_usd,cum_cost,in_tokens" > "$ITERCSV"

git rev-parse --verify "$OPT_BRANCH" >/dev/null 2>&1 || git branch "$OPT_BRANCH"
git checkout -q "$OPT_BRANCH" || { LOG "cannot checkout $OPT_BRANCH"; exit 1; }

best_score(){ python3 -c "import json;print(json.load(open('$BEST')).get('score',0))" 2>/dev/null || echo 0; }
wt_revert(){ git checkout -- . 2>/dev/null; git clean -fdq gate 2>/dev/null; }  # pre-commit (no reset!)

if [ ! -f "$BEST" ]; then
  LOG "no champion -> scoring baseline"
  base=$(bash optimizer/score.sh 2>>"$RESULTS_DIR/loop-logs/score.log" | python3 -c "import sys,json;print(json.load(sys.stdin).get('score',0))" 2>/dev/null || echo 0)
  python3 -c "import json;json.dump({'score':float('$base'),'note':'baseline'},open('$BEST','w'))"
  LOG "baseline champion=$base"
fi

SID=""; iter=0; no_improve=0; CUM=0
LOG "start. champion=$(best_score) model='${OPT_MODEL:-default}' branch=$OPT_BRANCH"

while [ "$iter" -lt "$MAX_ITERS" ]; do
  [ -f "$RESULTS_DIR/STOP" ] && { LOG "STOP -> exit"; rm -f "$RESULTS_DIR/STOP"; break; }
  iter=$((iter+1)); LOG "==== iter $iter (no_improve=$no_improve/$PATIENCE champion=$(best_score)) ===="
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

  # enforce the LOCKED contract: only gate/ changes are considered/committed.
  # (Scope to gate/ so supervisor files like optimizer/MONITOR.md and .gitignore
  # don't get mistaken for optimizer edits. ALLOWED_PATHS then locks within gate/.)
  changed=$(git status --porcelain -- gate | awk '{print $2}')
  [ -z "$changed" ] && { LOG "no gate changes -> skip measure"; continue; }
  viol=0
  for f in $changed; do ok=0; for p in $ALLOWED_PATHS; do case "$f" in $p/*) ok=1;; esac; done; [ "$ok" = 0 ] && { LOG "VIOLATION edit outside allowed: $f"; viol=1; }; done
  [ "$viol" = 1 ] && { wt_revert; no_improve=$((no_improve+1)); continue; }

  git add -A -- gate
  git commit -q -m "optimize(iter $iter): ${HYP:-mutation}" --author="optimizer <opt@flash-relay.local>"

  res=$(bash optimizer/score.sh 2>>"$RESULTS_DIR/loop-logs/score.log")
  score=$(echo "$res" | python3 -c "import sys,json;print(json.load(sys.stdin).get('score',0))" 2>/dev/null || echo 0)
  reason=$(echo "$res" | python3 -c "import sys,json;print(json.load(sys.stdin).get('reason','?'))" 2>/dev/null || echo '?')
  best=$(best_score); thresh=$(awk -v b="$best" -v e="$EPSILON" 'BEGIN{printf "%.4f",b*(1+e)}')
  LOG "measured score=$score ($reason) champion=$best promote-if>$thresh"

  if awk -v s="$score" 'BEGIN{exit !(s<=0)}'; then
    VERDICT="revert-fail"; git reset --hard HEAD~1 -q; no_improve=$((no_improve+1))
  elif awk -v s="$score" -v t="$thresh" 'BEGIN{exit !(s>t)}'; then
    VERDICT="promote"; python3 -c "import json;json.dump({'score':float('$score'),'hash':'$(git rev-parse HEAD)','note':'champion'},open('$BEST','w'))"; no_improve=0
    LOG "*** NEW CHAMPION score=$score (was $best) ***"
  else
    VERDICT="revert-regression"; git reset --hard HEAD~1 -q; no_improve=$((no_improve+1))
  fi
  CUM=$(awk -v a="$CUM" -v b="${cost:-0}" 'BEGIN{printf "%.4f",a+b}')
  utc=$(date -u +%FT%TZ)
  echo "$iter,$utc,$score,$best,$VERDICT,$reason,${cost:-0},$CUM,${intok:-0}" >> "$ITERCSV"
  echo "{\"ts\":\"$(date -u +'%F %T')\",\"iter\":$iter,\"score\":$score,\"champion\":$best,\"verdict\":\"$VERDICT\",\"reason\":\"$reason\",\"cost_usd\":${cost:-0},\"cum_cost_usd\":$CUM,\"in_tokens\":${intok:-0},\"hypothesis\":$(python3 -c "import json,sys;print(json.dumps(sys.argv[1]))" "$HYP" 2>/dev/null||echo '""')}" \
    | clickhouse-client --query "INSERT INTO $CH_DB.iterations FORMAT JSONEachRow" 2>/dev/null || true

  [ "$no_improve" -ge "$PATIENCE" ] && { LOG "plateau ($no_improve w/o improvement) -> stop"; break; }
done
LOG "ended at iter $iter. champion=$(best_score)."
