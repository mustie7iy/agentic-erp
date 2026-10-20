-- A statement file whose content changed on the server after it was
-- imported (T49d; ADR-0047): the fetch reads the file again before moving
-- it to the archive and, finding other content, leaves it and records the
-- change, with the content it now has as the reason; the next fetch
-- imports that content as a new file. Only a refusal and a change say why.
-- Both constraints are checked against the table's rows, which only
-- 000107's statuses hold.
ALTER TABLE payments.statement_fetch DROP CONSTRAINT statement_fetch_status_check,
  ADD CONSTRAINT statement_fetch_status_check CHECK (status IN ('imported', 'archived', 'refused', 'changed_after_import')),
  DROP CONSTRAINT statement_fetch_reason,
  ADD CONSTRAINT statement_fetch_reason CHECK ((status IN ('refused', 'changed_after_import')) = (reason IS NOT NULL));
