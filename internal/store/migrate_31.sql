-- v30 -> v31: a worker's model and thinking level set by hand (`piggery model`, top's picker) are kept
-- by a reload, which takes the rest from config.yaml again (1 = set by hand).
ALTER TABLE participants ADD COLUMN model_by_hand INTEGER NOT NULL DEFAULT 0;
ALTER TABLE participants ADD COLUMN thinking_by_hand INTEGER NOT NULL DEFAULT 0;
