-- v11 -> v12: batches the daemon counts for an adapter (Claude hooks): the harness's key of the
-- turn (prompt_id), when the turn ended (acked or not; completed_at stays the ack), and how many
-- times its end was blocked to show more mail. prompt_id NULL = a batch the harness numbers itself
-- (pi).
ALTER TABLE batches ADD COLUMN prompt_id TEXT;
ALTER TABLE batches ADD COLUMN ended_at INTEGER;
ALTER TABLE batches ADD COLUMN blocks INTEGER NOT NULL DEFAULT 0;
