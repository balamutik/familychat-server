CREATE TABLE message_reactions (
 message_id uuid NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
 user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 emoji text NOT NULL CHECK (octet_length(emoji) BETWEEN 1 AND 128),
 PRIMARY KEY (message_id,user_id)
);
