-- v1 -> v2: durable metadata of worker processes, one row per run.
CREATE TABLE processes (
  participant_id TEXT NOT NULL REFERENCES participants(id),
  run_id         TEXT NOT NULL,
  pid            INTEGER NOT NULL,
  pgid           INTEGER NOT NULL,
  start_time     INTEGER NOT NULL,          -- OS process start time, unix ms
  cmdline        TEXT NOT NULL,             -- JSON array
  started_at     INTEGER NOT NULL,
  exited_at      INTEGER,
  exit_code      INTEGER,                   -- -1 when killed by a signal
  PRIMARY KEY (participant_id, run_id)
);
