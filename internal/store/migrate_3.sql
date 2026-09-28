-- v2 -> v3: routing cc copies link to the message they copy.
ALTER TABLE messages ADD COLUMN cc_of TEXT REFERENCES messages(id);
CREATE INDEX messages_cc ON messages(cc_of);
