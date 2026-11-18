-- The revert is refused while a contract has followed its order: the
-- previous version knows no follow, so a contract's order values, price
-- and end would stand without the change that explains them, and an
-- ended contract would be taken for one that recognises its order.
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM revenue.contract_change) THEN
    RAISE EXCEPTION 'revenue.contract_change holds follows of sales orders: reverting would drop them'
      USING ERRCODE = 'check_violation';
  END IF;
END $$;
REVOKE UPDATE (net_amount, ended_at) ON revenue.performance_obligation FROM erp_app;
REVOKE UPDATE (transaction_price, changes, ended_at) ON revenue.contract FROM erp_app;

CREATE OR REPLACE FUNCTION revenue.check_obligation_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (SELECT k.status FROM revenue.contract k WHERE k.tenant_id = NEW.tenant_id AND k.id = NEW.contract_id FOR UPDATE) <> 'draft' THEN
    RAISE EXCEPTION 'performance obligation % of revenue contract %: an active contract''s obligations are fixed', NEW.obligation_no, NEW.contract_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'performance_obligation_fixed_once_active';
  END IF;
  RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION revenue.check_contract_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'draft' THEN
    RAISE EXCEPTION 'revenue contract %: only a draft changes', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'contract_fixed_once_active';
  END IF;
  IF NEW.status = 'active' AND NEW.transaction_price IS DISTINCT FROM
      (SELECT CASE WHEN bool_and(o.allocated_amount IS NOT NULL) THEN sum(o.allocated_amount) END
       FROM revenue.performance_obligation o WHERE o.tenant_id = NEW.tenant_id AND o.contract_id = NEW.id) THEN
    RAISE EXCEPTION 'revenue contract %: its obligations'' allocated amounts are not its transaction price', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'contract_allocated_in_full';
  END IF;
  RETURN NEW;
END $$;

DROP TABLE revenue.contract_change;
DROP FUNCTION revenue.check_contract_change_followed();
ALTER TABLE revenue.performance_obligation
  DROP CONSTRAINT performance_obligation_ended_unallocated,
  DROP COLUMN ended_at;
ALTER TABLE revenue.contract
  DROP CONSTRAINT contract_transaction_price_positive,
  ADD CONSTRAINT contract_transaction_price_positive CHECK (transaction_price > 0),
  DROP COLUMN ended_at,
  DROP COLUMN changes;
