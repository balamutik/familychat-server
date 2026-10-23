ALTER TABLE messages
  ADD COLUMN forwarded boolean NOT NULL DEFAULT false;
