-- A settlement reversed (T60i; ADR-0064 §3, plan-depth-1 §T60i): an
-- order's latest settlement's journal entry reversed through the ledger
-- on its own posting date, so the order holds again what the settlement
-- moved. Kept here once per settlement, with the reversal's entry (the
-- ledger's: no foreign key crosses a schema). Insert-only: the table has
-- no version, so the application is granted no UPDATE
-- (platform.secure_table), and never a DELETE; a reversal is not taken
-- back. Expand only: a new table, which the previous version neither
-- reads nor writes.
CREATE TABLE controlling.order_settlement_reversal (
  tenant_id         uuid NOT NULL,
  settlement_id     uuid NOT NULL,
  journal_entry_id  uuid NOT NULL,     -- ledger context: the reversal's entry
  document_number   text NOT NULL CONSTRAINT order_settlement_reversal_number CHECK (document_number <> ''),
  created_at        timestamptz NOT NULL DEFAULT now(),
  created_by        uuid NOT NULL,
  -- A settlement is reversed once.
  PRIMARY KEY (tenant_id, settlement_id),
  CONSTRAINT order_settlement_reversal_settlement FOREIGN KEY (tenant_id, settlement_id) REFERENCES controlling.order_settlement (tenant_id, id),
  -- An entry reverses one settlement.
  CONSTRAINT order_settlement_reversal_entry_once UNIQUE (tenant_id, journal_entry_id)
);
SELECT platform.secure_table('controlling.order_settlement_reversal'::regclass);
