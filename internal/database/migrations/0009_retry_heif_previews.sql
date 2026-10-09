-- Older libheif could not decode auxiliary image references in iPhone HEICs.
-- Retry exhausted jobs after upgrading the decoder, without touching ready
-- previews, active jobs, or originals that retention has already removed.
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
      'image/heic', 'image/heif', 'image/heic-sequence', 'image/heif-sequence'
    )
  RETURNING j.attachment_id
)
UPDATE attachments AS a
SET preview_state = 'pending'
FROM retry
WHERE a.id = retry.attachment_id;
