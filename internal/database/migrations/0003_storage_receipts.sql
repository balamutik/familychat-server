ALTER TABLE settings
  ADD COLUMN retention_days integer NOT NULL DEFAULT 0 CHECK (retention_days BETWEEN 0 AND 3650),
  ADD COLUMN max_file_bytes bigint NOT NULL DEFAULT 262144000 CHECK (max_file_bytes > 0);

ALTER TABLE attachments ADD COLUMN purged_at timestamptz;
CREATE INDEX attachments_unpurged_created_idx ON attachments(created_at) WHERE purged_at IS NULL;

ALTER TABLE chat_members ADD COLUMN delivered_seq bigint NOT NULL DEFAULT 0;
UPDATE chat_members SET delivered_seq=read_seq;
ALTER TABLE chat_members
  ADD CONSTRAINT chat_members_receipt_order CHECK (read_seq >= 0 AND delivered_seq >= read_seq);
