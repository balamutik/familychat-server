CREATE TABLE user_blocks (
 source_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 target_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(source_user_id,target_user_id),
 CHECK(source_user_id<>target_user_id)
);
CREATE INDEX user_blocks_target_idx ON user_blocks(target_user_id,source_user_id);
