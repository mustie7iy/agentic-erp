-- A revenue contract follows its sales order (T81e; ADR-0068, As built
-- T81e; 03-data-model §3.30). The order is changed in place (T78d) and
-- cancelled; the contract's consumer reads the order's SalesOrderChanged
-- and brings the contract to the order as it is: each obligation's order
-- value is its item's net value, the transaction price the order's net
-- value and, for an active contract, the shares are the new price
-- allocated by the stand-alone selling prices; a cancelled order ends
-- every obligation, with nothing allocated. Each follow is kept as a
-- numbered change with its values before and after, and moves the
-- contract's change count, which is how the contract's rules tell a
-- follow from a person's change. What a person judged (timing, service
-- period, the day named, the stand-alone selling price) a follow never
-- touches.
--
-- Expand only (06 §6.4): a column with a default, nullable columns, a
-- table, the grants a follow needs and a wider price check; the previous
-- version neither reads nor writes any of them, and the contracts it
-- creates, changes and activates stay valid.
ALTER TABLE revenue.contract
  ADD COLUMN changes  integer NOT NULL DEFAULT 0 CONSTRAINT contract_changes_check CHECK (changes >= 0),
  -- When the order was found cancelled: the contract takes nothing more.
  ADD COLUMN ended_at timestamptz,
  -- An ended contract's transaction price is zero, its order being
  -- cancelled; every other contract's is more than zero.
  DROP CONSTRAINT contract_transaction_price_positive,
  ADD CONSTRAINT contract_transaction_price_positive CHECK (transaction_price > 0 OR (ended_at IS NOT NULL AND transaction_price = 0));

-- An obligation ends when its order is cancelled (T78e: or its item
-- removed); it keeps its order value as it last was, and an active
-- contract's ended obligation has nothing allocated.
ALTER TABLE revenue.performance_obligation
  ADD COLUMN ended_at timestamptz,
  ADD CONSTRAINT performance_obligation_ended_unallocated CHECK (ended_at IS NULL OR allocated_amount IS NULL OR allocated_amount = 0);

-- A follow of a contract (T81e): numbered from 1 within its contract, its
-- cause, the order's version and change count it followed, the
-- transaction price before and after and, as a JSON array, each
-- obligation it changed (its number with its order value and allocated
-- amount before and after, and whether it ended). Written once and never
-- changed.
CREATE TABLE revenue.contract_change (
  tenant_id                 uuid NOT NULL,
  id                        uuid NOT NULL,
  contract_id               uuid NOT NULL,
  change_no                 integer NOT NULL CONSTRAINT contract_change_no_positive CHECK (change_no > 0),
  cause                     text NOT NULL CONSTRAINT contract_change_cause_check CHECK (cause IN ('sales_order_changed', 'sales_order_cancelled')),
  sales_order_version       integer NOT NULL CONSTRAINT contract_change_order_version_positive CHECK (sales_order_version > 0),
  sales_order_changes       integer NOT NULL CONSTRAINT contract_change_order_changes_check CHECK (sales_order_changes >= 0),
  transaction_price_before  bigint NOT NULL CONSTRAINT contract_change_price_before_check CHECK (transaction_price_before >= 0),
  transaction_price_after   bigint NOT NULL CONSTRAINT contract_change_price_after_check CHECK (transaction_price_after >= 0),
  obligations               jsonb NOT NULL CONSTRAINT contract_change_obligations_check CHECK (jsonb_typeof(obligations) = 'array'),
  created_at                timestamptz NOT NULL DEFAULT now(),
  created_by                uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT contract_change_once UNIQUE (tenant_id, contract_id, change_no),
  CONSTRAINT contract_change_contract_fkey FOREIGN KEY (tenant_id, contract_id) REFERENCES revenue.contract (tenant_id, id)
);
CREATE TRIGGER contract_change_immutable BEFORE UPDATE OR DELETE ON revenue.contract_change
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('revenue.contract_change'::regclass);

-- A contract's own changes: a person's, while it is a draft (T81b), which
-- leaves its price, its change count and its end as they are; and a
-- follow (T81e), of a draft or an active contract, which moves the change
-- count by one to a change kept in revenue.contract_change and keeps the
-- status. An ended contract changes no more. An active contract's
-- obligations each hold their share, and the shares are the transaction
-- price (ADR-0068 §7).
CREATE OR REPLACE FUNCTION revenue.check_contract_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.ended_at IS NOT NULL THEN
    RAISE EXCEPTION 'revenue contract %: its sales order is cancelled, so it changes no more', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'contract_ended';
  END IF;
  IF NEW.changes <> OLD.changes THEN
    IF NEW.changes <> OLD.changes + 1 OR NEW.status <> OLD.status OR NOT EXISTS (SELECT FROM revenue.contract_change c
        WHERE c.tenant_id = NEW.tenant_id AND c.contract_id = NEW.id AND c.change_no = NEW.changes) THEN
      RAISE EXCEPTION 'revenue contract %: a follow moves the change count by one to a kept change and keeps the status', OLD.id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'contract_follow_kept';
    END IF;
  ELSE
    IF OLD.status <> 'draft' THEN
      RAISE EXCEPTION 'revenue contract %: only a draft changes', OLD.id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'contract_fixed_once_active';
    END IF;
    IF NEW.transaction_price <> OLD.transaction_price OR NEW.ended_at IS NOT NULL THEN
      RAISE EXCEPTION 'revenue contract %: its price and its end move only with a kept follow', OLD.id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'contract_follow_kept';
    END IF;
  END IF;
  IF NEW.status = 'active' AND NEW.transaction_price IS DISTINCT FROM
      (SELECT CASE WHEN bool_and(o.allocated_amount IS NOT NULL) THEN sum(o.allocated_amount) END
       FROM revenue.performance_obligation o WHERE o.tenant_id = NEW.tenant_id AND o.contract_id = NEW.id) THEN
    RAISE EXCEPTION 'revenue contract %: its obligations'' allocated amounts are not its transaction price', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'contract_allocated_in_full';
  END IF;
  RETURN NEW;
END $$;

-- An obligation is written and changed by a person only while its
-- contract is a draft (T81b), and by a follow (T81e) only in the
-- transaction that keeps the contract's next change: its order value, its
-- allocated amount and its end, never what a person judged. An ended
-- obligation, and one of an ended contract, changes no more. The
-- contract's row is locked for the check, so a change of an obligation,
-- a follow and the contract's activation wait for one another.
CREATE OR REPLACE FUNCTION revenue.check_obligation_change() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  k record;
  following boolean;
BEGIN
  SELECT c.status, c.changes, c.ended_at INTO k FROM revenue.contract c WHERE c.tenant_id = NEW.tenant_id AND c.id = NEW.contract_id FOR UPDATE;
  IF k.ended_at IS NOT NULL OR (TG_OP = 'UPDATE' AND OLD.ended_at IS NOT NULL) THEN
    RAISE EXCEPTION 'performance obligation % of revenue contract %: it has ended, so it changes no more', NEW.obligation_no, NEW.contract_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'contract_ended';
  END IF;
  following := EXISTS (SELECT FROM revenue.contract_change c
    WHERE c.tenant_id = NEW.tenant_id AND c.contract_id = NEW.contract_id AND c.change_no = k.changes + 1);
  IF following THEN
    IF TG_OP = 'UPDATE' AND (NEW.timing <> OLD.timing OR NEW.service_from IS DISTINCT FROM OLD.service_from
        OR NEW.service_to IS DISTINCT FROM OLD.service_to OR NEW.satisfied_on IS DISTINCT FROM OLD.satisfied_on
        OR NEW.standalone_selling_price <> OLD.standalone_selling_price) THEN
      RAISE EXCEPTION 'performance obligation % of revenue contract %: a follow keeps what a person judged', NEW.obligation_no, NEW.contract_id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'performance_obligation_judgement_kept';
    END IF;
    RETURN NEW;
  END IF;
  IF k.status <> 'draft' THEN
    RAISE EXCEPTION 'performance obligation % of revenue contract %: an active contract''s obligations are fixed', NEW.obligation_no, NEW.contract_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'performance_obligation_fixed_once_active';
  END IF;
  IF TG_OP = 'UPDATE' AND (NEW.net_amount <> OLD.net_amount OR NEW.ended_at IS NOT NULL) THEN
    RAISE EXCEPTION 'performance obligation % of revenue contract %: its order value and its end move only with a kept follow', NEW.obligation_no, NEW.contract_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'contract_follow_kept';
  END IF;
  RETURN NEW;
END $$;

GRANT UPDATE (transaction_price, changes, ended_at) ON revenue.contract TO erp_app;
GRANT UPDATE (net_amount, ended_at) ON revenue.performance_obligation TO erp_app;
