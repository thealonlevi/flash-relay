-- flash-relay optimizer ClickHouse schema. Apply: clickhouse-client --multiquery < optimizer/schema.sql
CREATE DATABASE IF NOT EXISTS flashrelay;

-- one row per optimizer iteration: score + verdict + economics + the agent's hypothesis.
CREATE TABLE IF NOT EXISTS flashrelay.iterations (
  ts            DateTime,
  iter          UInt32,
  score         Float64,          -- 1e9 / instr_per_conn (higher = better)
  champion      Float64,          -- best score so far at decision time
  verdict       LowCardinality(String),  -- promote | revert-regression | revert-fail
  reason        LowCardinality(String),  -- ok | build_fail | ring_test_fail | audit_fail | two_fd_fail | drop_high | duplex_broken | ...
  cost_usd      Float64,
  cum_cost_usd  Float64,
  in_tokens     UInt64,
  hypothesis    String
) ENGINE = MergeTree ORDER BY (ts);
