CREATE TABLE invitations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    token text NOT NULL UNIQUE CHECK (length(token) = 64),
    server_name text NOT NULL,
    server_url text NOT NULL,
    created_by uuid REFERENCES users(id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);
CREATE INDEX invitations_created ON invitations (created_at DESC);
