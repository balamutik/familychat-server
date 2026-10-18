ALTER TABLE push_devices ADD COLUMN platform text NOT NULL DEFAULT 'ios';
ALTER TABLE push_devices ADD COLUMN fcm_token text;
ALTER TABLE push_devices DROP CONSTRAINT push_devices_check;
ALTER TABLE push_devices ADD CONSTRAINT push_devices_provider_check CHECK (
 (platform='ios' AND fcm_token IS NULL AND (alert_token IS NOT NULL OR voip_token IS NOT NULL)) OR
 (platform='android' AND fcm_token IS NOT NULL AND alert_token IS NULL AND voip_token IS NULL)
);
ALTER TABLE push_jobs DROP CONSTRAINT push_jobs_kind_check;
ALTER TABLE push_jobs ADD CONSTRAINT push_jobs_kind_check CHECK(kind IN ('alert','voip','fcm_message','fcm_call','fcm_call_state'));
