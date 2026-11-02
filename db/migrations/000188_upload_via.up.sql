-- Where an upload's bytes go (ADR-0020 As built 2026-11-01, PO 2026-11-01):
-- 'store' to the object store's presigned URL when the store presigns,
-- 'api' to the API's own signed path, which streams them into the store.
-- The API's path accepts the bytes of an 'api' upload, or of any upload
-- when the store cannot presign (the directory store), so the rows written
-- before this column, 'store' by default, keep the URL they were given.
ALTER TABLE payables.upload ADD COLUMN via text NOT NULL DEFAULT 'store' CHECK (via IN ('store', 'api'));
