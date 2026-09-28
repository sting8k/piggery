-- v18 -> v19: the run of a worker whose harness is running a turn no delivered batch started
-- (Claude's background task notification), as its runtime driver reported it. While set for the
-- current run the worker stays working; NULL none.
ALTER TABLE participants ADD COLUMN unbatched_run TEXT;
