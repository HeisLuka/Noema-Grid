-- 0082_space_files_hash_idx.sql — index the content hash.
--
-- Every attachment serve (GET /api/files/{space}/{hash}) looks a blob up by
-- content_hash and had NO index to use: a seq scan over space_files on every
-- image embed in every rendered page. The shareable file page (/f/{hash}/{name})
-- adds a PREFIX lookup on the same column, so the opclass is text_pattern_ops —
-- it serves `LIKE 'abc%'` as a range scan regardless of the database collation,
-- and equality rides it too.
CREATE INDEX space_files_hash
    ON space_files (content_hash text_pattern_ops)
 WHERE deleted_at IS NULL;
