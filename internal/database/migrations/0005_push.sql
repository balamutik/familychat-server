CREATE TABLE push_devices (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  session_id uuid NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  client_server_id uuid NOT NULL,
  device_id uuid NOT NULL,
  alert_token text,
  voip_token text,
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(user_id,device_id),
  CHECK (alert_token IS NOT NULL OR voip_token IS NOT NULL)
);
CREATE INDEX push_devices_user_idx ON push_devices(user_id);

CREATE TABLE push_jobs (
  id bigserial PRIMARY KEY,
  device_id uuid NOT NULL REFERENCES push_devices(id) ON DELETE CASCADE,
  kind text NOT NULL CHECK (kind IN ('alert','voip')),
  payload jsonb NOT NULL,
  attempts integer NOT NULL DEFAULT 0,
  available_at timestamptz NOT NULL DEFAULT now(),
  claimed_until timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX push_jobs_ready_idx ON push_jobs(available_at,id);
