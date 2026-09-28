-- v16 -> v17: the protocol version the participant's adapter sent at its latest identify (0: an
-- adapter from before the exchange). ps and doctor show a session whose adapter differs from the
-- daemon. NULL = not identified since.
ALTER TABLE participants ADD COLUMN protocol_version INTEGER;
