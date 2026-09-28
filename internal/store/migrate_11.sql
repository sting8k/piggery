-- v10 -> v11: the worker's thinking level chosen by the chain (as model, v8), passed to the harness
-- as declared; and the session's current thinking level as its driver reports it (as session_model,
-- v10). NULL = none/unknown.
ALTER TABLE participants ADD COLUMN thinking TEXT;
ALTER TABLE participants ADD COLUMN session_thinking TEXT;
