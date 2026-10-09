ALTER TABLE calls ADD COLUMN connected_at timestamptz;
-- Historical rows and calls started by older clients have no reliable media evidence.
ALTER TABLE calls ADD COLUMN media_tracking boolean NOT NULL DEFAULT false;
CREATE INDEX calls_history_caller_idx ON calls(caller_id, created_at DESC, id DESC);
CREATE INDEX calls_history_callee_idx ON calls(callee_id, created_at DESC, id DESC);
