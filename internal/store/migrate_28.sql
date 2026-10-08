-- v27 -> v28: closed gate. 1 = the team's (teams) or the solo's (participants) gate is closed to other
-- teams and solos; a member's own column is unused while it is in a team. Existing rows stay open.
ALTER TABLE teams ADD COLUMN gate_closed INTEGER NOT NULL DEFAULT 0;
ALTER TABLE participants ADD COLUMN gate_closed INTEGER NOT NULL DEFAULT 0;
