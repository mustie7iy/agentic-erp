DELETE FROM identity.sod_rule WHERE permission_a = 'controlling.order.settle' OR permission_b = 'controlling.order.settle';
DELETE FROM identity.role_permission WHERE permission_code = 'controlling.order.settle';
DELETE FROM ref.permission WHERE code = 'controlling.order.settle';
-- The previous version knows no closed order: one closed since is released
-- again, as it was before it was closed, while the trigger is away.
DROP TRIGGER internal_order_lifecycle ON controlling.internal_order;
UPDATE controlling.internal_order SET status = 'released', version = version + 1 WHERE status = 'closed';
CREATE OR REPLACE FUNCTION controlling.check_internal_order() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'created' THEN
      RAISE EXCEPTION 'internal order % starts as created', NEW.id USING ERRCODE = 'check_violation', CONSTRAINT = 'internal_order_starts_created';
    END IF;
    RETURN NEW;
  END IF;
  IF (NEW.legal_entity_id, NEW.code) IS DISTINCT FROM (OLD.legal_entity_id, OLD.code) THEN
    RAISE EXCEPTION 'internal order % keeps its company and code', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'internal_order_identity_kept';
  END IF;
  IF OLD.status = 'released' AND NEW.status <> 'released' THEN
    RAISE EXCEPTION 'internal order % stays released', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'internal_order_stays_released';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER internal_order_lifecycle BEFORE INSERT OR UPDATE ON controlling.internal_order
  FOR EACH ROW EXECUTE FUNCTION controlling.check_internal_order();
ALTER TABLE controlling.internal_order DROP CONSTRAINT internal_order_status_check;
ALTER TABLE controlling.internal_order ADD CONSTRAINT internal_order_status_check CHECK (status IN ('created', 'released'));
DROP TABLE controlling.order_settlement;
