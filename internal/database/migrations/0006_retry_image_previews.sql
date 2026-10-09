-- Previously unsupported image codecs may have exhausted their preview jobs.
-- Retry only missing previews; leave already available previews untouched.
WITH retry AS (
  UPDATE preview_jobs AS j
  SET state = 'pending', attempts = 0, lease_until = NULL
  FROM attachments AS a
  WHERE j.attachment_id = a.id
    AND j.state IN ('failed', 'unsupported')
    AND a.preview_state IN ('failed', 'unsupported')
    AND a.preview_key IS NULL
    AND a.purged_at IS NULL
    AND lower(trim(split_part(a.content_type, ';', 1))) IN (
      'image/jpeg', 'image/png', 'image/apng', 'image/gif', 'image/webp',
      'image/heic', 'image/heif', 'image/heic-sequence', 'image/heif-sequence',
      'image/avif', 'image/avif-sequence', 'image/bmp', 'image/x-ms-bmp',
      'image/tiff', 'image/x-icon', 'image/vnd.microsoft.icon', 'image/ico',
      'image/svg+xml'
    )
  RETURNING j.attachment_id
)
UPDATE attachments AS a
SET preview_state = 'pending'
FROM retry
WHERE a.id = retry.attachment_id;
