-- Scheduling agreements (T83a; ADR-0070 §1-§3, plan-breadth-week2 §T83a):
-- a contract of its own kind whose delivery schedule is entered once it
-- is active. A contract has a kind, `contract` (the default: every
-- contract there is, and what the previous version creates) or
-- `scheduling_agreement`, fixed when it is created. A schedule line asks
-- for a quantity of one of the agreement's items on a date within the
-- agreement's validity, in that item's unit; lines are numbered 1 to n
-- per agreement in the order entered. Its value is not stored: it is
-- quantity × the item's unit price ÷ price unit, and an active
-- contract's items never change (contract_item_editable). Expand only: a
-- column with a default and a new table, which the previous version
-- neither reads nor writes (06 §6.4).
ALTER TABLE procurement.contract
  ADD COLUMN kind text NOT NULL DEFAULT 'contract'
    CONSTRAINT contract_kind_check CHECK (kind IN ('contract', 'scheduling_agreement'));

-- A contract keeps its kind: a contract with orders against it does not
-- become a scheduling agreement, nor an agreement with a schedule a
-- contract.
CREATE FUNCTION procurement.check_contract_kind() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.kind IS DISTINCT FROM OLD.kind THEN
    RAISE EXCEPTION 'contract % keeps its kind, %', OLD.document_number, OLD.kind
      USING ERRCODE = 'check_violation', CONSTRAINT = 'contract_kind_kept';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_contract_kind() OWNER TO erp_owner;
CREATE TRIGGER contract_kind_kept BEFORE UPDATE ON procurement.contract
  FOR EACH ROW EXECUTE FUNCTION procurement.check_contract_kind();

CREATE TABLE procurement.schedule_line (
  tenant_id         uuid NOT NULL,
  id                uuid NOT NULL,
  contract_id       uuid NOT NULL,
  line_no           integer NOT NULL,            -- 1 to n per contract (schedule_line_numbered_in_order)
  contract_item_no  integer NOT NULL,
  delivery_date     date NOT NULL,
  quantity          numeric(20,6) NOT NULL CONSTRAINT schedule_line_quantity_check CHECK (quantity > 0),
  version           integer NOT NULL DEFAULT 1,
  created_at        timestamptz NOT NULL DEFAULT now(),
  created_by        uuid NOT NULL,
  updated_at        timestamptz NOT NULL DEFAULT now(),
  updated_by        uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT schedule_line_numbered_once UNIQUE (tenant_id, contract_id, line_no),
  -- A line asks for an item of its own agreement.
  CONSTRAINT schedule_line_item_fkey FOREIGN KEY (tenant_id, contract_id, contract_item_no)
    REFERENCES procurement.contract_item (tenant_id, contract_id, item_no)
);
CREATE TRIGGER schedule_line_touch BEFORE UPDATE ON procurement.schedule_line FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list: an agreement's lines by delivery date, in the order its pages
-- are read.
CREATE INDEX schedule_line_by_date ON procurement.schedule_line (tenant_id, contract_id, delivery_date, id);
SELECT platform.secure_table('procurement.schedule_line'::regclass);
-- Entering a line is the only write: nothing changes a line yet, and
-- nothing is deleted (T83b grants what a call-off sets).
REVOKE UPDATE ON procurement.schedule_line FROM erp_app;

-- Only an active scheduling agreement takes schedule lines, for its live
-- items, on dates within its validity, numbered in the order entered. The
-- contract is read FOR SHARE, so a line is not entered beside a change of
-- the contract in progress (its termination, its expiry).
CREATE FUNCTION procurement.check_schedule_line() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  c procurement.contract%ROWTYPE;
  last integer;
BEGIN
  SELECT * INTO c FROM procurement.contract WHERE tenant_id = NEW.tenant_id AND id = NEW.contract_id FOR SHARE;
  IF NOT FOUND THEN
    RETURN NEW; -- schedule_line_item_fkey refuses it
  END IF;
  IF c.kind <> 'scheduling_agreement' THEN
    RAISE EXCEPTION 'contract % is not a scheduling agreement: it takes no schedule line', c.document_number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'schedule_line_of_agreement';
  END IF;
  IF c.status <> 'active' THEN
    RAISE EXCEPTION 'scheduling agreement % is %: only an active one takes schedule lines', c.document_number, c.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'schedule_line_of_active_agreement';
  END IF;
  IF NEW.delivery_date NOT BETWEEN c.valid_from AND c.valid_to THEN
    RAISE EXCEPTION 'scheduling agreement % is valid from % to %: a schedule line is due within it', c.document_number, c.valid_from, c.valid_to
      USING ERRCODE = 'check_violation', CONSTRAINT = 'schedule_line_within_validity';
  END IF;
  IF EXISTS (SELECT FROM procurement.contract_item i WHERE i.tenant_id = NEW.tenant_id AND i.contract_id = NEW.contract_id
      AND i.item_no = NEW.contract_item_no AND i.is_removed) THEN
    RAISE EXCEPTION 'item % of scheduling agreement % was removed', NEW.contract_item_no, c.document_number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'schedule_line_of_live_item';
  END IF;
  SELECT coalesce(max(line_no), 0) INTO last FROM procurement.schedule_line WHERE tenant_id = NEW.tenant_id AND contract_id = NEW.contract_id;
  IF NEW.line_no <> last + 1 THEN
    RAISE EXCEPTION 'schedule line % of scheduling agreement %: its lines are numbered 1 to n, the next is %', NEW.line_no, c.document_number, last + 1
      USING ERRCODE = 'check_violation', CONSTRAINT = 'schedule_line_numbered_in_order';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_schedule_line() OWNER TO erp_owner;
CREATE TRIGGER schedule_line_checked BEFORE INSERT ON procurement.schedule_line
  FOR EACH ROW EXECUTE FUNCTION procurement.check_schedule_line();
