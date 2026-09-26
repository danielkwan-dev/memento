-- Distinguishes "Letterboxd refused to serve this host" from other failures.
--
-- The remedy differs and is worth telling the user about: uploading a data export
-- skips the paginated profile pages, which are the requests that get gated. A
-- boolean rather than string matching on the error message, so the UI has
-- something stable to branch on.
ALTER TABLE jobs ADD COLUMN blocked BOOLEAN NOT NULL DEFAULT false;
