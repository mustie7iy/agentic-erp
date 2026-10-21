-- The bank files a house bank has accepted, by content hash (T44c;
-- ADR-0038; 03-data-model §3.10). A row is written in its own transaction
-- right after the bank takes the file, so a transmission whose own commit
-- is lost still leaves it; the retry finds the hash and sends nothing. A
-- file rendered again after a lost acknowledgement has the same content and
-- hash, so the key is the hash, per house bank: the channel (connector_id,
-- the integration context's, so no foreign key) may change while the bank
-- stays the same. Insert-only: what a bank took is never taken back; the row
-- is the upload's audit record (ADR-0011). No index backs the bank_file
-- foreign key: bank files are never deleted or re-keyed, and the erasure of
-- a tenant deletes this table's rows first.
CREATE TABLE payments.sent_bank_file (
  tenant_id      uuid NOT NULL,
  house_bank_id  uuid NOT NULL,
  sha256         bytea NOT NULL CHECK (length(sha256) = 32),
  bank_file_id   uuid NOT NULL,
  connector_id   uuid,  -- null: the server's directory (DirConnector)
  sent_at        timestamptz NOT NULL DEFAULT now(),
  sent_by        uuid NOT NULL,
  PRIMARY KEY (tenant_id, house_bank_id, sha256),
  FOREIGN KEY (tenant_id, house_bank_id) REFERENCES payments.house_bank (tenant_id, id),
  FOREIGN KEY (tenant_id, bank_file_id) REFERENCES payments.bank_file (tenant_id, id)
);
CREATE TRIGGER sent_bank_file_immutable BEFORE UPDATE OR DELETE ON payments.sent_bank_file FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('payments.sent_bank_file'::regclass);
