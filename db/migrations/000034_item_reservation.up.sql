-- Open-item reservations (F5, 03-data-model §3.7): an open item another
-- context is about to settle (a payment run's) is reserved in the ledger,
-- so the ledger's own manual clearing and reversal refuse it without the
-- ledger reading the owner's tables (payments depends on ledger, never the
-- other way round). The owner reserves and frees its items through the
-- ledger's Postings port, in the transaction that changes its document; a
-- settlement by the owner ends the reservation of the items it clears.
CREATE TABLE ledger.item_reservation (
  tenant_id         uuid NOT NULL,
  owner_id          uuid NOT NULL,                  -- the owner's document: a payment run
  fiscal_year       smallint NOT NULL,
  journal_entry_id  uuid NOT NULL,
  line_no           integer NOT NULL,
  owner_context     text NOT NULL CHECK (owner_context IN ('payments')),
  owner_reference   text NOT NULL,                  -- what refusals name: the run's document number
  created_at        timestamptz NOT NULL DEFAULT now(),
  created_by        uuid NOT NULL,
  released_at       timestamptz,
  released_by       uuid,
  PRIMARY KEY (tenant_id, owner_id, fiscal_year, journal_entry_id, line_no),
  FOREIGN KEY (tenant_id, fiscal_year, journal_entry_id, line_no) REFERENCES ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no),
  CHECK ((released_at IS NULL) = (released_by IS NULL))
);
-- An item is held by at most one owner at a time.
CREATE UNIQUE INDEX item_reserved_once ON ledger.item_reservation (tenant_id, fiscal_year, journal_entry_id, line_no) WHERE released_at IS NULL;

-- A reservation is only ever released, once; nothing else changes.
CREATE FUNCTION ledger.check_reservation_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.released_at IS NOT NULL THEN
    RAISE EXCEPTION 'reservation of item %/%/% is already released', OLD.fiscal_year, OLD.journal_entry_id, OLD.line_no USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_reservation_update() OWNER TO erp_owner;
CREATE TRIGGER item_reservation_released_once BEFORE UPDATE ON ledger.item_reservation FOR EACH ROW EXECUTE FUNCTION ledger.check_reservation_update();
SELECT platform.secure_table('ledger.item_reservation'::regclass);
GRANT UPDATE (released_at, released_by) ON ledger.item_reservation TO erp_app;

-- The items live runs hold today: a proposed run's, and a releasing run's
-- whose payment is not posted yet (payments.Reservations.RunOf). Live runs
-- are few, so this is not a long backfill.
INSERT INTO ledger.item_reservation (tenant_id, owner_id, fiscal_year, journal_entry_id, line_no, owner_context, owner_reference, created_by)
SELECT i.tenant_id, r.id, i.item_fiscal_year, i.item_entry_id, i.item_line_no, 'payments', r.document_number, r.proposed_by
FROM payments.payment_item i
JOIN payments.payment p ON p.tenant_id = i.tenant_id AND p.id = i.payment_id
JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id
WHERE i.is_active AND (r.status IN ('proposing', 'proposed') OR (r.status = 'releasing' AND p.journal_entry_id IS NULL));
