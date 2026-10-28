-- GR/IR clearing of closed orders (T72e, SAP MR11; 03-data-model §3.7,
-- §3.8, ADR-0034 As built). A closed order whose GR/IR position is not
-- zero (received and invoiced quantities that will never agree) is
-- cleared by a document of the procurement context: proposed, approved by
-- a controller through a gr_ir_clearing approval request, and posted then
-- through ledger.Postings against the price difference account.
--
-- The ledger keeps the clearing apart from receipts and invoices: the
-- quantity a clearing posts is neither received nor invoiced, so a
-- position's quantity is invoiced + cleared - received and the report
-- shows each. The entries that clear are recorded in
-- ledger.gr_ir_clearing_entry, which the projection and the key-date
-- report read. Expand only: the previous version neither reads cleared
-- (default 0, every position stays as it was) nor writes the new tables;
-- is_open is the same for every existing row (cleared = 0).

ALTER TABLE ledger.gr_ir_position ADD COLUMN cleared numeric(20,6) NOT NULL DEFAULT 0;
ALTER TABLE ledger.gr_ir_position ALTER COLUMN is_open
  SET EXPRESSION AS (received <> invoiced + cleared OR amount_tc <> 0 OR amount_lc <> 0 OR amount_gc <> 0);

-- The journal entries that clear GR/IR positions (insert-only): their
-- GR/IR lines' quantities are cleared, not received or invoiced.
CREATE TABLE ledger.gr_ir_clearing_entry (
  tenant_id         uuid NOT NULL,
  fiscal_year       smallint NOT NULL,
  journal_entry_id  uuid NOT NULL,
  created_at        timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, fiscal_year, journal_entry_id),
  FOREIGN KEY (tenant_id, fiscal_year, journal_entry_id) REFERENCES ledger.journal_entry (tenant_id, fiscal_year, id)
);
CREATE TRIGGER gr_ir_clearing_entry_insert_only BEFORE UPDATE OR DELETE ON ledger.gr_ir_clearing_entry
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('ledger.gr_ir_clearing_entry'::regclass);

-- A clearing of a closed order's GR/IR positions: pending while its
-- approval request is, then posted (naming its WE entry), rejected or
-- cancelled (its requester withdrew it). One pending per order.
CREATE TABLE procurement.gr_ir_clearing (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  purchase_order_id    uuid NOT NULL,
  legal_entity_id      uuid NOT NULL,     -- the order's (tenancy context)
  status               text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'posted', 'rejected', 'cancelled')),
  reason               text NOT NULL CHECK (btrim(reason) <> '' AND length(reason) <= 2000),
  posting_date         date NOT NULL,
  currency             char(3) NOT NULL REFERENCES ref.currency,   -- the order's
  -- The price difference account it posts against (ledger context), as
  -- the approver was shown it.
  offset_account_id      uuid NOT NULL,
  offset_account_number  text NOT NULL,
  offset_account_name    text NOT NULL,
  approval_request_id  uuid NOT NULL,     -- workflow context
  journal_entry_id     uuid,              -- ledger context, once posted
  journal_entry_year   smallint,
  journal_entry_number text,
  decision_comment     text,
  version              integer NOT NULL DEFAULT 1,
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, purchase_order_id) REFERENCES procurement.purchase_order (tenant_id, id),
  CONSTRAINT gr_ir_clearing_posted CHECK ((status = 'posted') = (journal_entry_id IS NOT NULL AND journal_entry_year IS NOT NULL AND journal_entry_number IS NOT NULL))
);
CREATE UNIQUE INDEX gr_ir_clearing_one_pending ON procurement.gr_ir_clearing (tenant_id, purchase_order_id) WHERE status = 'pending';
CREATE INDEX gr_ir_clearing_by_order ON procurement.gr_ir_clearing (tenant_id, purchase_order_id, created_at, id);
CREATE TRIGGER gr_ir_clearing_touch BEFORE UPDATE ON procurement.gr_ir_clearing FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- A clearing starts pending and is decided once: pending -> posted,
-- rejected or cancelled. What it clears and posts against never changes:
-- the decision sets its status, the entry it posted and the comment.
CREATE FUNCTION procurement.check_gr_ir_clearing() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'pending' THEN
      RAISE EXCEPTION 'a GR/IR clearing starts pending, not %', NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'gr_ir_clearing_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.status <> 'pending' OR NEW.status = 'pending' THEN
    RAISE EXCEPTION 'GR/IR clearing % cannot go from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'gr_ir_clearing_transition';
  END IF;
  IF (NEW.purchase_order_id, NEW.legal_entity_id, NEW.reason, NEW.posting_date, NEW.currency, NEW.offset_account_id,
      NEW.offset_account_number, NEW.offset_account_name, NEW.approval_request_id, NEW.created_at, NEW.created_by)
      IS DISTINCT FROM (OLD.purchase_order_id, OLD.legal_entity_id, OLD.reason, OLD.posting_date, OLD.currency, OLD.offset_account_id,
      OLD.offset_account_number, OLD.offset_account_name, OLD.approval_request_id, OLD.created_at, OLD.created_by) THEN
    RAISE EXCEPTION 'GR/IR clearing % changes only its status', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'gr_ir_clearing_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_gr_ir_clearing() OWNER TO erp_owner;
CREATE TRIGGER gr_ir_clearing_transition BEFORE INSERT OR UPDATE ON procurement.gr_ir_clearing
  FOR EACH ROW EXECUTE FUNCTION procurement.check_gr_ir_clearing();
SELECT platform.secure_table('procurement.gr_ir_clearing'::regclass);

-- The positions a clearing clears, as they stood when it was proposed:
-- the quantity (invoiced + cleared - received, signed as the position)
-- and the balance in the order's currency. Insert-only.
CREATE TABLE procurement.gr_ir_clearing_item (
  tenant_id         uuid NOT NULL,
  gr_ir_clearing_id uuid NOT NULL,
  po_item_no        integer NOT NULL,
  quantity          numeric(20,6) NOT NULL,
  uom_code          text NOT NULL REFERENCES ref.unit_of_measure,
  amount            bigint NOT NULL,      -- minor units of the clearing's currency
  PRIMARY KEY (tenant_id, gr_ir_clearing_id, po_item_no),
  FOREIGN KEY (tenant_id, gr_ir_clearing_id) REFERENCES procurement.gr_ir_clearing (tenant_id, id),
  CONSTRAINT gr_ir_clearing_item_open CHECK (quantity <> 0 OR amount <> 0)
);
CREATE TRIGGER gr_ir_clearing_item_insert_only BEFORE UPDATE OR DELETE ON procurement.gr_ir_clearing_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('procurement.gr_ir_clearing_item'::regclass);

-- Proposing is an accountant's; approving is a controller's (an approval
-- permission, which agents cannot hold).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('procurement.gr_ir.clear', 'Propose clearing the residual GR/IR positions of closed purchase orders', false),
  ('procurement.gr_ir.clear.approve', 'Approve the clearing of residual GR/IR positions of closed purchase orders', true);
