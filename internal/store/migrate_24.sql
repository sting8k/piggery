-- v23 -> v24: joined_at is when the participant entered the team it is in now (found, admit, reopen
-- moving a solo in); NULL means it was created in the team, so created_at is that moment. The
-- gate is the live member that joined earliest, and a solo made before the founder but admitted
-- later joined later.
-- Backfill: the ts of its `admitted` event (or of the `team_up` event of its reopen) for its team;
-- else, a member made before its team existed is the founder: the team's created_at.
ALTER TABLE participants ADD COLUMN joined_at INTEGER;
UPDATE participants SET joined_at = COALESCE(
  (SELECT MAX(e.ts) FROM events e WHERE e.participant = participants.id AND e.team_id = participants.team_id
     AND e.type IN ('admitted', 'team_up')),
  (SELECT t.created_at FROM teams t WHERE t.id = participants.team_id AND participants.created_at < t.created_at))
WHERE team_id IS NOT NULL;
