ALTER TABLE users
  ADD COLUMN avatar_key text UNIQUE,
  ADD COLUMN avatar_content_type text,
  ADD COLUMN avatar_size_bytes bigint,
  ADD CONSTRAINT users_avatar_columns CHECK (
    (avatar_key IS NULL AND avatar_content_type IS NULL AND avatar_size_bytes IS NULL) OR
    (avatar_key IS NOT NULL AND avatar_content_type IN ('image/jpeg','image/png') AND avatar_size_bytes BETWEEN 1 AND 5242880)
  );

CREATE TABLE avatar_gc (
  object_key text PRIMARY KEY,
  created_at timestamptz NOT NULL DEFAULT now()
);
