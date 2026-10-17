ALTER TABLE messages ADD COLUMN deleted_at timestamptz;
CREATE TABLE message_hidden (
 message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
 user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(message_id,user_id)
);
