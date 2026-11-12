-- Internal orders (T60a; ADR-0064 §1, plan-breadth-week1 §T60a): the order
-- master of the controlling context, the second real cost object beside
-- the ledger's cost center (ADR-0050 §2, §5). An order belongs to a
-- company, has a code unique in it, a name and a responsible cost center,
-- from which postings to the order take their profit center and segment
-- (T60b), and a status: created, then released for posting by a person
-- holding controlling.order.release. legal_entity_id is the tenancy
-- context's and the cost center the ledger's: no foreign key crosses a
-- schema; the controlling context checks them through
-- ledger.AssignmentBalances. Expand only: a new table, which the previous
-- version neither reads nor writes.
CREATE TABLE controlling.internal_order (
  tenant_id                   uuid NOT NULL,
  id                          uuid NOT NULL,
  legal_entity_id             uuid NOT NULL,     -- tenancy context
  code                        text NOT NULL CHECK (code ~ '^[0-9A-Z_-]{1,12}$'),
  name                        text NOT NULL CHECK (btrim(name) <> '' AND length(name) <= 100),
  responsible_cost_center_id  uuid NOT NULL,     -- ledger context
  status                      text NOT NULL DEFAULT 'created' CHECK (status IN ('created', 'released')),
  version                     integer NOT NULL DEFAULT 1,
  created_at                  timestamptz NOT NULL DEFAULT now(),
  created_by                  uuid NOT NULL,
  updated_at                  timestamptz NOT NULL DEFAULT now(),
  updated_by                  uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT internal_order_code_once UNIQUE (tenant_id, legal_entity_id, code)
);
CREATE TRIGGER internal_order_touch BEFORE UPDATE ON controlling.internal_order FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('controlling.internal_order'::regclass);

-- An order is created in status created, keeps its company and code, and
-- is released once: lines posted to it name it by id and were let in
-- because it was released, so a release is never taken back.
CREATE FUNCTION controlling.check_internal_order() RETURNS trigger
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
ALTER FUNCTION controlling.check_internal_order() OWNER TO erp_owner;
CREATE TRIGGER internal_order_lifecycle BEFORE INSERT OR UPDATE ON controlling.internal_order
  FOR EACH ROW EXECUTE FUNCTION controlling.check_internal_order();

-- Reading, creating and releasing (ADR-0064 §1). Releasing lets costs be
-- posted to the order: an approval permission, which agents cannot hold.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('controlling.order.read', 'View internal orders', false),
  ('controlling.order.write', 'Create internal orders', false),
  ('controlling.order.release', 'Release internal orders for posting', true);
