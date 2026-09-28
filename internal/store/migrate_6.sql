-- v5 -> v6: the reserved address human became notify, a hook-only channel with no inbox. Mail to
-- human is renamed and marked done (acked): nothing reads or acks it anymore, and a held one still
-- runs the notify hook if an admin releases it. Mail sent as human keeps from_id 'human' (plain
-- history).
UPDATE messages SET to_id='notify', acked_at=COALESCE(acked_at, created_at) WHERE to_id='human';
