-- v22 -> v23: mail wakes a gone member of an open team as a headless worker.
-- wake_seq: the newest seq among the unacked mail that already caused such a wake; a wake needs
-- newer mail, so a worker that dies with the same mail unacked is not woken by it again.
-- session_mode: the mode a person's session had when it was woken (NULL: never woken, or a worker the
-- daemon spawned); the person's own join takes the participant back and restores that mode.
ALTER TABLE participants ADD COLUMN wake_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE participants ADD COLUMN session_mode TEXT;
