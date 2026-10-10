ALTER TABLE users ADD COLUMN display_name text NOT NULL DEFAULT '' CHECK (char_length(display_name) <= 64);
