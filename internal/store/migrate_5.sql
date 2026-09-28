-- v4 -> v5: watch keeps only silent_for; the turns_without_tool state goes.
ALTER TABLE participants DROP COLUMN turns_no_tool;
ALTER TABLE participants DROP COLUMN no_tool_streak;
