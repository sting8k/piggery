-- v14 -> v15: the hash of the role (team, role, name) whose card the participant's session last
-- got. A harness with no system prompt of piggery's (a Claude session a person opened) gets the new
-- card with the next turn's mail after a role change. NULL = none given.
ALTER TABLE participants ADD COLUMN card_hash TEXT;
