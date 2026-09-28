-- v17 -> v18: how a batch a runtime driver delivered ended without a completion: 'cancelled' (the
-- harness dropped it on its own), 'abort' (a piggery abort ended it first), 'deliver_error' (the
-- driver could not deliver it). Core redelivers after a harness cancel only while no other batch of
-- the run was cancelled that way since its last completed batch. NULL: completed, or not a
-- delivered batch.
ALTER TABLE batches ADD COLUMN end_reason TEXT;
