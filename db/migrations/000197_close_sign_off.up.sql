-- A period close's sign-offs (T64d; ADR-0058 §5, §7; 03-data-model §3.22):
-- :sign-off re-runs the close's checks, locks each covered ledger's closed
-- period through the ledger's period lock (T32), and records who signed
-- it off, when, the note and, per covered ledger, its period and whether
-- this sign-off locked it (locked: the ledger's code and name as they were
-- then). A close signed off again after a covered period was unlocked has
-- a second row. A sign-off is a record: never updated or deleted
-- (platform.forbid_mutation, and no grant beyond SELECT and INSERT).
-- signed_by is the identity context's principal, the ledgers and periods
-- in locked the ledger context's: no foreign key leaves the schema.
-- Expand only: a new table and permission, which the previous version
-- neither reads nor writes.
CREATE TABLE closing.sign_off (
  tenant_id   uuid NOT NULL,
  id          uuid NOT NULL,
  close_id    uuid NOT NULL,
  signed_by   uuid NOT NULL,              -- identity context: a person
  signed_at   timestamptz NOT NULL DEFAULT now(),
  note        text CHECK (char_length(note) BETWEEN 1 AND 2000),
  locked      jsonb NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, close_id) REFERENCES closing.period_close (tenant_id, id),
  -- One entry per covered ledger, at most the 20 a close covers.
  CONSTRAINT sign_off_locked CHECK (jsonb_typeof(locked) = 'array' AND jsonb_array_length(locked) BETWEEN 1 AND 20)
);
-- A close's sign-offs, oldest first; the latest is the close's.
CREATE INDEX sign_off_by_close ON closing.sign_off (tenant_id, close_id, signed_at, id);
CREATE TRIGGER sign_off_immutable BEFORE UPDATE OR DELETE ON closing.sign_off FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('closing.sign_off'::regclass);

-- Signing off a period close (ADR-0058 §13); it locks periods, so the
-- operation also needs ledger.period.lock.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('closing.close.sign_off', 'Sign off period closes, locking their periods', false);
