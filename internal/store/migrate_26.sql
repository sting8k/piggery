-- v25 -> v26: "whose session is this" is its own fact.
-- person: 1 when the row is a person's session (set when it joins, a wake never changes it), 0 for a
-- worker the daemon spawned. mode keeps one meaning besides the harness's own mode: 'headless' is "the
-- daemon runs this row now" (a spawned worker, or a person's session a mail woke). session_mode, the
-- mode a woken person's row had, goes: the join that takes the session back sets the mode it reports.
-- Backfill: a row is a person's unless it is headless with no session_mode (spawned).
ALTER TABLE participants ADD COLUMN person INTEGER NOT NULL DEFAULT 0;
UPDATE participants SET person = 1 WHERE COALESCE(mode,'') <> 'headless' OR session_mode IS NOT NULL;
ALTER TABLE participants DROP COLUMN session_mode;
