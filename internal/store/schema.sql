-- Schema v1. Step 0 of store.migrations; later versions are migrate_N.sql. Do not edit: v1 DB files
-- already exist.
CREATE TABLE teams (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, model_name TEXT NOT NULL,
  manifest TEXT NOT NULL,
  overrides TEXT NOT NULL DEFAULT '{}',
  root_cwd TEXT NOT NULL, created_at INTEGER NOT NULL, closed_at INTEGER
);

CREATE TABLE participants (
  id            TEXT PRIMARY KEY,
  run_id        TEXT NOT NULL,
  kind          TEXT NOT NULL,
  harness       TEXT, mode TEXT,
  name          TEXT NOT NULL,
  cwd           TEXT NOT NULL,
  team_id       TEXT REFERENCES teams(id),
  role          TEXT,
  reports_to    TEXT REFERENCES participants(id),
  spawned_by    TEXT REFERENCES participants(id),
  state         TEXT NOT NULL,
  state_since   INTEGER NOT NULL,
  last_turn_end INTEGER,
  last_activity INTEGER NOT NULL,
  token_hash    TEXT NOT NULL,
  harness_ref   TEXT,
  created_at    INTEGER NOT NULL
);
CREATE INDEX participants_team ON participants(team_id);
CREATE INDEX participants_cwd  ON participants(cwd);
CREATE UNIQUE INDEX participants_team_name ON participants(team_id, name);

CREATE TABLE messages (
  id            TEXT PRIMARY KEY,
  seq           INTEGER NOT NULL UNIQUE,
  client_msg_id TEXT,
  team_id       TEXT REFERENCES teams(id),
  from_id       TEXT NOT NULL,
  to_id         TEXT NOT NULL,
  kind          TEXT,
  thread_id     TEXT NOT NULL,
  reply_to      TEXT REFERENCES messages(id),
  expects_reply INTEGER NOT NULL DEFAULT 0,
  op            TEXT,
  target        TEXT REFERENCES messages(id),
  body          TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  acked_at      INTEGER,
  held_reason   TEXT
);
CREATE UNIQUE INDEX messages_idem   ON messages(from_id, client_msg_id) WHERE client_msg_id IS NOT NULL;
CREATE INDEX messages_inbox         ON messages(to_id, acked_at, seq);
CREATE INDEX messages_thread        ON messages(thread_id, seq);
CREATE INDEX messages_reply         ON messages(reply_to, from_id);
CREATE INDEX messages_target        ON messages(target);
CREATE INDEX messages_board         ON messages(team_id, seq) WHERE to_id='board';

CREATE TABLE deliveries (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  message_id   TEXT NOT NULL REFERENCES messages(id),
  run_id       TEXT NOT NULL,
  batch_seq    INTEGER,
  delivered_at INTEGER NOT NULL,
  acked_at     INTEGER
);
CREATE INDEX deliveries_open ON deliveries(run_id, batch_seq) WHERE acked_at IS NULL;

-- Completion units per run. A row with completed_at set is closed: no new deliveries into it.
-- Source of truth for "batch closed" (events are audit only).
CREATE TABLE batches (
  run_id       TEXT NOT NULL,
  batch_seq    INTEGER NOT NULL,
  opened_at    INTEGER NOT NULL,
  completed_at INTEGER,
  PRIMARY KEY (run_id, batch_seq)
);

CREATE VIEW pins AS
  SELECT m.* FROM messages m
  WHERE m.to_id='board' AND (m.op IS NULL OR m.op='replace')
    AND NOT EXISTS (SELECT 1 FROM messages x WHERE x.target = m.id AND x.op IN ('replace','remove'));

CREATE TABLE events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL,
  type TEXT NOT NULL,
  participant TEXT, team_id TEXT, run_id TEXT, ref_id TEXT,
  payload TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX events_team ON events(team_id, seq);
CREATE INDEX events_part ON events(participant, type, seq);

CREATE TABLE timers (
  id TEXT PRIMARY KEY, team_id TEXT, owner TEXT NOT NULL, target TEXT NOT NULL,
  fire_at INTEGER NOT NULL, every_ms INTEGER,
  condition TEXT,
  payload TEXT NOT NULL, active INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX timers_due ON timers(active, fire_at);

CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);
