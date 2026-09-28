-- v3 -> v4: state that replaces the per-turn events. The current run's streak of turns without a
-- tool call, and an id for that streak (the watch incident key); both reset on a turn with a tool
-- call and on a new run.
ALTER TABLE participants ADD COLUMN turns_no_tool INTEGER NOT NULL DEFAULT 0;
ALTER TABLE participants ADD COLUMN no_tool_streak TEXT;
