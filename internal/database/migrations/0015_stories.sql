CREATE TABLE stories (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 user_id uuid NOT NULL REFERENCES users(id),
 object_key text NOT NULL UNIQUE,
 filename text NOT NULL,
 content_type text NOT NULL,
 size_bytes bigint NOT NULL CHECK(size_bytes>0),
 stored_size_bytes bigint NOT NULL CHECK(stored_size_bytes>0),
 encrypted boolean NOT NULL DEFAULT false,
 caption text NOT NULL DEFAULT '',
 state text NOT NULL DEFAULT 'uploading' CHECK(state IN ('uploading','ready','failed')),
 created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz NOT NULL DEFAULT now()+interval '24 hours',
 upload_deadline timestamptz NOT NULL DEFAULT now()+interval '30 minutes',
 deleted_at timestamptz,
 purged_at timestamptz
);
CREATE INDEX stories_feed_idx ON stories(created_at DESC,id DESC) WHERE state='ready' AND deleted_at IS NULL AND purged_at IS NULL;
CREATE INDEX stories_owner_storage_idx ON stories(user_id) WHERE purged_at IS NULL;
CREATE TABLE story_views (
 story_id uuid NOT NULL REFERENCES stories(id) ON DELETE CASCADE,
 user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 viewed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(story_id,user_id)
);
