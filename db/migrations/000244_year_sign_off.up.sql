-- A year close's sign-offs (T64m6; ADR-0060 §8, §12; 03-data-model §3.22):
-- :signOff re-runs the year close's checks, needs every regular period of
-- the year locked in each covered ledger and every special period closed
-- or locked, locks each closed special period through the ledger's period
-- lock (T32), and records who signed it off (id, and name as it was then),
-- when, the note and, per covered ledger, its special periods and whether
-- this sign-off locked each (locked: the ledger's code and name as they
-- were then). A year close signed off again after a period was unlocked
-- has a second row. As closing.sign_off (000199): a record, never updated
-- or deleted (platform.forbid_mutation, and no grant beyond SELECT and
-- INSERT). signed_by is the identity context's principal, the ledgers and
-- periods in locked the ledger context's: no foreign key leaves the schema.
-- The operation reuses closing.close.sign_off (000199). Expand only: a new
-- table, which the previous version neither reads nor writes.
CREATE TABLE closing.year_sign_off (
  tenant_id       uuid NOT NULL,
  id              uuid NOT NULL,
  close_id        uuid NOT NULL,
  signed_by       uuid NOT NULL,          -- identity context: a person
  signed_by_name  text NOT NULL CHECK (btrim(signed_by_name) <> ''),
  signed_at       timestamptz NOT NULL DEFAULT now(),
  note            text CHECK (char_length(note) BETWEEN 1 AND 2000),
  locked          jsonb NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, close_id) REFERENCES closing.year_close (tenant_id, id),
  -- One entry per covered ledger, at most the 20 a close covers.
  CONSTRAINT year_sign_off_locked CHECK (jsonb_typeof(locked) = 'array' AND jsonb_array_length(locked) BETWEEN 1 AND 20)
);
-- A year close's sign-offs, oldest first; the latest is the close's.
CREATE INDEX year_sign_off_by_close ON closing.year_sign_off (tenant_id, close_id, signed_at, id);
CREATE TRIGGER year_sign_off_immutable BEFORE UPDATE OR DELETE ON closing.year_sign_off FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('closing.year_sign_off'::regclass);
