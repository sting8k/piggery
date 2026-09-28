-- v12 -> v13: what the participant's harness supports, a JSON array of core.Cap* names: declared by
-- the adapter at identify for a session, by the runtime driver at spawn/resume for a headless
-- worker. NULL = nothing declared.
ALTER TABLE participants ADD COLUMN capabilities TEXT;
