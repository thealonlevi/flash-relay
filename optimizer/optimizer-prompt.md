Make ONE focused change to the io_uring relay hot path (`gate/internal/uring/` or `gate/cmd/relay-uring/`) to reduce CPU instructions per relayed connection.

Steps:
1. Read the current ring + relay code and `optimizer/results/RECENT.csv` (recent attempts: score, verdict, reason — don't repeat what was just reverted; build on what was promoted).
2. Form a hypothesis for the single highest-value reduction in instr/conn right now.
3. Apply that one change. Keep it compiling (`CGO_ENABLED=0 go build ./gate/...`) and keep `go test ./gate/internal/uring/` passing.
4. Reply with 1–2 sentences: what you changed and why it should cut instr/conn.

Do not edit anything outside the two allowed paths. Do not run the scorer, git, or the optimizer. The referee measures and judges your change.
