-- v6 -> v7: leaving a team. left_at marks a participant row that left its team (found elsewhere);
-- rerouted_from keeps the original recipient of unacked mail moved to the team's new gate.
ALTER TABLE participants ADD COLUMN left_at INTEGER;
ALTER TABLE messages ADD COLUMN rerouted_from TEXT;
