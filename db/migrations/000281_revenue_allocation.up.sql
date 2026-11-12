-- Price allocation (T81b; ADR-0068 §7; 03-data-model §3.29): while a
-- revenue contract is a draft, the revenue accountant sets per obligation
-- whether it is satisfied at a point in time or over time, an over-time
-- obligation's service period and its stand-alone selling price;
-- activating the contract allocates its transaction price to the
-- obligations in proportion to those prices and fixes it. The allocated
-- amounts add up to the transaction price to the minor unit, which the
-- database holds the activation to. Expand only (06 §6.4): three nullable
-- columns, a wider status and one permission, which the previous version
-- neither reads nor writes; every obligation it wrote is satisfied at a
-- point in time without a service period, as the new rule asks.
ALTER TABLE revenue.contract
  DROP CONSTRAINT contract_status_check,
  ADD CONSTRAINT contract_status_check CHECK (status IN ('draft', 'active'));

-- service_from and service_to are the period an over-time obligation is
-- satisfied over, both days included; a point-in-time obligation has
-- none. allocated_amount is the obligation's share of the contract's
-- transaction price in minor units of its currency, set when the contract
-- is activated and never before.
ALTER TABLE revenue.performance_obligation
  ADD COLUMN service_from date,
  ADD COLUMN service_to date,
  ADD COLUMN allocated_amount bigint CONSTRAINT performance_obligation_allocated_check CHECK (allocated_amount >= 0),
  ADD CONSTRAINT performance_obligation_service_period CHECK (
    (timing = 'over_time' AND service_from IS NOT NULL AND service_to IS NOT NULL AND service_from <= service_to)
    OR (timing = 'point_in_time' AND service_from IS NULL AND service_to IS NULL));

-- A contract changes only while it is a draft: its version moves with
-- each change of its obligations, and activating it is its last change.
-- An active contract's obligations each hold their share, and the shares
-- are the transaction price (ADR-0068 §7).
CREATE FUNCTION revenue.check_contract_change() RETURNS trigger
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
ALTER FUNCTION revenue.check_contract_change() OWNER TO erp_owner;
CREATE TRIGGER contract_fixed_once_active BEFORE UPDATE ON revenue.contract
  FOR EACH ROW EXECUTE FUNCTION revenue.check_contract_change();
GRANT UPDATE (status, version, updated_by) ON revenue.contract TO erp_app;

-- An obligation is written and changed only while its contract is a
-- draft. The contract's row is locked for the check, so a change of an
-- obligation and the contract's activation wait for one another and the
-- activation's sum is of what stays.
CREATE FUNCTION revenue.check_obligation_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (SELECT k.status FROM revenue.contract k WHERE k.tenant_id = NEW.tenant_id AND k.id = NEW.contract_id FOR UPDATE) <> 'draft' THEN
    RAISE EXCEPTION 'performance obligation % of revenue contract %: an active contract''s obligations are fixed', NEW.obligation_no, NEW.contract_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'performance_obligation_fixed_once_active';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION revenue.check_obligation_change() OWNER TO erp_owner;
CREATE TRIGGER performance_obligation_fixed_once_active BEFORE INSERT OR UPDATE ON revenue.performance_obligation
  FOR EACH ROW EXECUTE FUNCTION revenue.check_obligation_change();
GRANT UPDATE (timing, service_from, service_to, standalone_selling_price, allocated_amount) ON revenue.performance_obligation TO erp_app;

-- Activating a revenue contract (ADR-0068 §6).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('revenue.contract.activate', 'Activate revenue contracts, which allocates their transaction price and fixes them', false);
