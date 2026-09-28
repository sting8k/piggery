-- v9 -> v10: the session's current model as its driver reports it (identify model, presence "model"); a
-- worker whose nearest non-headless ancestor it is inherits it when both run the same harness.
-- participants.model (v8) now holds the model chosen by the whole chain; NULL = the harness default.
ALTER TABLE participants ADD COLUMN session_model TEXT;
