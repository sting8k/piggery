-- v7 -> v8: the model chosen at spawn (explicit, else the role's spawn.model; NULL = the profile's
-- default), reused by resume unless it passes another.
ALTER TABLE participants ADD COLUMN model TEXT;
