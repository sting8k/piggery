-- v13 -> v14: a session a person opened is known by its host process ("claude:<pid>:<start time>"),
-- which its hooks and MCP server share and which outlives a session id (Claude's /clear starts a
-- new id in the same process). participant_refs: the other session ids of a participant, so
-- resuming any of them finds it. NULL host = none reported.
ALTER TABLE participants ADD COLUMN host TEXT;
CREATE TABLE participant_refs (
  ref TEXT PRIMARY KEY,
  participant_id TEXT NOT NULL
);
