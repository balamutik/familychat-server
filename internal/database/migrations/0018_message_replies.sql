ALTER TABLE messages
  ADD COLUMN reply_to_message_id uuid REFERENCES messages(id) ON DELETE SET NULL;

CREATE INDEX messages_reply_to_idx ON messages(reply_to_message_id)
  WHERE reply_to_message_id IS NOT NULL;
