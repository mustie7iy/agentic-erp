-- An archive file written in parts (ADR-0079 §7, review:batch75): the
-- archiver writes a year a part at a time, each part an object of its own,
-- so a year of any size holds at most two parts in memory. A file within
-- one part is one object at object_key, as before, and parts is null; a
-- file of more is its parts, in order, at least two (name, SHA-256, size
-- and entries each), object_key the first's, while sha256 and size_bytes
-- stay the whole file's. Expand-only: every run before this has none.
ALTER TABLE records.archiving_run
  ADD COLUMN parts jsonb,
  ADD CONSTRAINT archiving_run_parts_check CHECK (parts IS NULL OR (object_key IS NOT NULL AND jsonb_typeof(parts) = 'array' AND jsonb_array_length(parts) >= 2));
GRANT UPDATE (parts) ON records.archiving_run TO erp_app;
