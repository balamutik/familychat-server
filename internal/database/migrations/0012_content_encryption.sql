ALTER TABLE messages ADD COLUMN encrypted boolean NOT NULL DEFAULT false;
ALTER TABLE attachments ADD COLUMN encrypted boolean NOT NULL DEFAULT false;
ALTER TABLE attachments ADD COLUMN preview_size_bytes bigint NOT NULL DEFAULT 0 CHECK (preview_size_bytes >= 0);

-- Keys live in a separate persistent volume, never in the database or S3.
CREATE TABLE content_encryption (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  key_id text NOT NULL,
  migrated boolean NOT NULL DEFAULT false
);
CREATE TABLE encryption_gc (object_key text PRIMARY KEY, not_before timestamptz NOT NULL DEFAULT now());

DROP INDEX messages_search_idx;
DROP INDEX messages_trgm_idx;

CREATE INDEX messages_unencrypted_idx ON messages(id) WHERE NOT encrypted;
CREATE INDEX attachments_unencrypted_idx ON attachments(id) WHERE NOT encrypted;
