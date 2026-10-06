-- v26 -> v27: taskforce. parent_id is the participant that called the team up
-- with spawn template=: NULL for an ordinary team. idle_noticed is the start of the idle stretch the
-- caller was last told about (taskforce.idle_for), so one stretch is told once. No REFERENCES: gc deletes
-- a team's participants before the team.
ALTER TABLE teams ADD COLUMN parent_id TEXT;
ALTER TABLE teams ADD COLUMN idle_noticed INTEGER;
