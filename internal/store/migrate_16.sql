-- v15 -> v16: the session id a session a person opened runs now (its latest SessionStart or join).
-- harness_ref stays the first; participant_refs holds the others. A wake names it (Codex's `codex
-- queue --thread` needs it), and a session_end for another id (Codex ends the old id ~30 s after
-- /clear started the new one) is ignored. NULL = unknown.
ALTER TABLE participants ADD COLUMN session_ref TEXT;
