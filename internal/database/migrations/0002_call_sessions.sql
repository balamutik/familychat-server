ALTER TABLE calls ADD COLUMN caller_session_id uuid REFERENCES sessions(id);
ALTER TABLE calls ADD COLUMN caller_disconnected_at timestamptz;
ALTER TABLE calls ADD COLUMN callee_disconnected_at timestamptz;
