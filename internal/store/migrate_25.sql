-- v24 -> v25: the team's gate is stored. gate_id is the member that is the gate:
-- the founder (found), the reopener (reopen), or, when the gate leaves, the next member by join order
-- (event gate_moved). A member that is only gone stays the gate; team down keeps it. NULL: no member
-- can be the gate (none left, or none with a role that has send). No REFERENCES: gc deletes a team's
-- participants before the team.
-- Backfill is Go (core Reconcile): the role's `send` tool is in the team's manifest, not in a column.
ALTER TABLE teams ADD COLUMN gate_id TEXT;
