-- Import jobs carry their uploaded export with them.
--
-- The archive lives in the jobs row rather than object storage: it is a few
-- hundred KB of CSV, it is needed exactly once by whichever worker claims the
-- job, and keeping it here means the import path needs no S3 bucket, no
-- credentials, and no cleanup job -- deleting the job row deletes the payload.
ALTER TABLE jobs ADD COLUMN payload BYTEA;

-- Guard against an oversized upload reaching the database at all.
ALTER TABLE jobs ADD CONSTRAINT jobs_payload_size
    CHECK (payload IS NULL OR octet_length(payload) <= 33554432);
