CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE users (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  login text NOT NULL UNIQUE CHECK (login ~ '^[a-z0-9_]{3,32}$'),
  password_hash text NOT NULL,
  role text NOT NULL DEFAULT 'user' CHECK (role IN ('admin','user')),
  disabled boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE settings (
  singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  registration_enabled boolean NOT NULL DEFAULT false
);
INSERT INTO settings(singleton, registration_enabled) VALUES(true,false);

CREATE TABLE sessions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash bytea NOT NULL UNIQUE,
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_user_idx ON sessions(user_id);

CREATE TABLE ws_tickets (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  token_hash bytea NOT NULL UNIQUE,
  expires_at timestamptz NOT NULL,
  consumed_at timestamptz
);

CREATE TABLE chats (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind text NOT NULL CHECK (kind IN ('direct','group')),
  title text NOT NULL DEFAULT '',
  direct_user_low uuid REFERENCES users(id),
  direct_user_high uuid REFERENCES users(id),
  next_seq bigint NOT NULL DEFAULT 1,
  deleted_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK ((kind='direct' AND direct_user_low IS NOT NULL AND direct_user_high IS NOT NULL AND direct_user_low < direct_user_high) OR
         (kind='group' AND direct_user_low IS NULL AND direct_user_high IS NULL))
);
CREATE UNIQUE INDEX chats_direct_pair_idx ON chats(direct_user_low,direct_user_high) WHERE kind='direct';

CREATE TABLE chat_members (
  chat_id uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id),
  role text NOT NULL CHECK(role IN ('owner','admin','member')),
  read_seq bigint NOT NULL DEFAULT 0,
  joined_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(chat_id,user_id)
);
CREATE INDEX chat_members_user_idx ON chat_members(user_id);

CREATE TABLE messages (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  chat_id uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  seq bigint NOT NULL,
  sender_id uuid NOT NULL REFERENCES users(id),
  client_message_id uuid NOT NULL,
  body text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(chat_id,seq),
  UNIQUE(chat_id,sender_id,client_message_id)
);
CREATE INDEX messages_chat_seq_idx ON messages(chat_id,seq DESC);
CREATE INDEX messages_search_idx ON messages USING gin (to_tsvector('russian',body));
CREATE INDEX messages_trgm_idx ON messages USING gin (body gin_trgm_ops);

CREATE TABLE attachments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  chat_id uuid NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  uploader_id uuid NOT NULL REFERENCES users(id),
  object_key text NOT NULL UNIQUE,
  preview_key text UNIQUE,
  filename text NOT NULL,
  content_type text NOT NULL,
  size_bytes bigint NOT NULL CHECK(size_bytes >= 0),
  preview_state text NOT NULL DEFAULT 'pending' CHECK(preview_state IN ('pending','processing','ready','failed','unsupported')),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX attachments_filename_trgm_idx ON attachments USING gin (filename gin_trgm_ops);

CREATE TABLE message_attachments (
  message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  attachment_id uuid NOT NULL UNIQUE REFERENCES attachments(id),
  PRIMARY KEY(message_id,attachment_id)
);

CREATE TABLE upload_reservations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id),
  chat_id uuid NOT NULL REFERENCES chats(id),
  object_key text NOT NULL UNIQUE,
  reserved_bytes bigint NOT NULL CHECK(reserved_bytes > 0),
  state text NOT NULL CHECK(state IN ('uploading','complete','failed')),
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX upload_reservations_pending_idx ON upload_reservations(expires_at) WHERE state='uploading';

CREATE TABLE preview_jobs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  attachment_id uuid NOT NULL UNIQUE REFERENCES attachments(id) ON DELETE CASCADE,
  attempts int NOT NULL DEFAULT 0,
  state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','processing','ready','failed','unsupported')),
  lease_until timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE events (
  id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  chat_id uuid REFERENCES chats(id) ON DELETE CASCADE,
  kind text NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_chat_idx ON events(chat_id,id);

CREATE TABLE calls (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  chat_id uuid NOT NULL REFERENCES chats(id),
  caller_id uuid NOT NULL REFERENCES users(id),
  callee_id uuid NOT NULL REFERENCES users(id),
  accepted_session_id uuid REFERENCES sessions(id),
  kind text NOT NULL CHECK(kind IN ('audio','video')),
  state text NOT NULL CHECK(state IN ('ringing','accepted','ended','rejected','missed','cancelled')),
  created_at timestamptz NOT NULL DEFAULT now(),
  accepted_at timestamptz,
  ended_at timestamptz,
  CHECK(caller_id <> callee_id)
);
CREATE INDEX calls_active_caller_idx ON calls(caller_id) WHERE state IN ('ringing','accepted');
CREATE INDEX calls_active_callee_idx ON calls(callee_id) WHERE state IN ('ringing','accepted');
