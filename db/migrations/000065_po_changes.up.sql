-- Purchase order changes (T47e, 03-data-model §3.8). A released order is
-- changed only by an order change: a draft of its header fields and its
-- complete items, submitted (and approved when it raises the order's
-- total, T47f), then applied in one transaction that names it on the
-- order (applying_change_id), writes the items, moves the order's
-- revision on by one and clears the name, and records what the order read
-- before. A released order's items and header change only while a change
-- is applied, never their identity, and never below what was received;
-- the valuation price of an item already valued never changes (000061),
-- so a change of price reaches GR/IR only through invoices.
ALTER TABLE procurement.purchase_order
  ADD COLUMN revision integer NOT NULL DEFAULT 1 CONSTRAINT purchase_order_revision_positive CHECK (revision > 0),
  ADD COLUMN applying_change_id uuid,
  ADD CONSTRAINT purchase_order_applying_released CHECK (applying_change_id IS NULL OR status = 'released');

CREATE TABLE procurement.purchase_order_change (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  purchase_order_id    uuid NOT NULL,
  status               text NOT NULL DEFAULT 'draft'
                       CHECK (status IN ('draft', 'pending_approval', 'applied', 'rejected', 'cancelled')),
  reason               text NOT NULL CHECK (length(reason) BETWEEN 1 AND 2000),
  base_revision        integer NOT NULL,       -- the order's revision it changes
  revision             integer,                -- the revision it made, once applied
  proposed             jsonb NOT NULL,         -- the order's header fields and complete items as changed
  previous             jsonb,                  -- the order as it read before, once applied
  total_before         bigint NOT NULL,        -- minor units of the order's currency
  total_after          bigint NOT NULL,
  approval_request_id  uuid,                   -- workflow context
  version              integer NOT NULL DEFAULT 1,
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, purchase_order_id) REFERENCES procurement.purchase_order (tenant_id, id),
  UNIQUE (tenant_id, purchase_order_id, revision),
  CONSTRAINT purchase_order_change_applied CHECK ((status = 'applied') = (revision IS NOT NULL AND previous IS NOT NULL)),
  -- what the order read is an object of its fields, never empty
  CONSTRAINT purchase_order_change_previous CHECK (previous IS NULL OR (jsonb_typeof(previous) = 'object' AND previous <> '{}'::jsonb)),
  CONSTRAINT purchase_order_change_request CHECK (status NOT IN ('pending_approval', 'rejected') OR approval_request_id IS NOT NULL),
  CONSTRAINT purchase_order_change_revision CHECK (status <> 'applied' OR revision = base_revision + 1)
);
CREATE UNIQUE INDEX purchase_order_one_open_change ON procurement.purchase_order_change (tenant_id, purchase_order_id)
  WHERE status IN ('draft', 'pending_approval');
CREATE TRIGGER purchase_order_change_touch BEFORE UPDATE ON procurement.purchase_order_change FOR EACH ROW EXECUTE FUNCTION platform.touch();
ALTER TABLE procurement.purchase_order ADD CONSTRAINT purchase_order_applying_change
  FOREIGN KEY (tenant_id, applying_change_id) REFERENCES procurement.purchase_order_change (tenant_id, id);

-- A change moves draft -> pending_approval -> applied or rejected, back to
-- draft when its request is withdrawn, straight to applied when it needs
-- no approval, or to cancelled; it changes only as a draft, and is decided
-- only on the request it names. Each changes the revision it is based on
-- and makes the next one.
CREATE FUNCTION procurement.check_order_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'draft' THEN
      RAISE EXCEPTION 'an order change starts as a draft, not %', NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_change_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF (OLD.status, NEW.status) NOT IN (('draft', 'draft'), ('draft', 'pending_approval'), ('draft', 'applied'), ('draft', 'cancelled'),
      ('pending_approval', 'applied'), ('pending_approval', 'rejected'), ('pending_approval', 'draft')) THEN
    RAISE EXCEPTION 'order change % cannot go from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_change_transition';
  END IF;
  IF (OLD.status, NEW.status) <> ('draft', 'draft') AND (NEW.purchase_order_id, NEW.proposed, NEW.reason, NEW.base_revision, NEW.total_before, NEW.total_after)
     IS DISTINCT FROM (OLD.purchase_order_id, OLD.proposed, OLD.reason, OLD.base_revision, OLD.total_before, OLD.total_after) THEN
    RAISE EXCEPTION 'order change % changes only while it is a draft', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_change_frozen';
  END IF;
  IF OLD.status = 'pending_approval' AND NEW.status IN ('applied', 'rejected') AND NEW.approval_request_id IS DISTINCT FROM OLD.approval_request_id THEN
    RAISE EXCEPTION 'order change % is decided only on the request it names', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_change_request_named';
  END IF;
  IF NEW.status = 'applied' AND OLD.status <> 'applied' AND NOT EXISTS (SELECT FROM procurement.purchase_order
      WHERE tenant_id = NEW.tenant_id AND id = NEW.purchase_order_id AND applying_change_id = NEW.id) THEN
    RAISE EXCEPTION 'order change % is applied only as its order applies it', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_change_applying';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_order_change() OWNER TO erp_owner;
CREATE TRIGGER purchase_order_change_transition BEFORE INSERT OR UPDATE ON procurement.purchase_order_change
  FOR EACH ROW EXECUTE FUNCTION procurement.check_order_change();

-- A released order's header changes only while it names the change being
-- applied, an open change of its own revision; its revision, a draft's
-- too, moves on by one exactly as the name is cleared, once the change
-- reads applied at that revision. A change is applied only while its
-- order names it, so the two move together. The application locks the
-- change FOR UPDATE before naming it (T47f): the check below reads it
-- unlocked. Its supplier, currency, legal entity, contract
-- and requisition never change once it left draft.
CREATE FUNCTION procurement.check_order_revision() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'draft' AND (NEW.supplier_id, NEW.currency, NEW.legal_entity_id, NEW.contract_id, NEW.requisition_id, NEW.purchasing_org_id,
      NEW.document_number) IS DISTINCT FROM (OLD.supplier_id, OLD.currency, OLD.legal_entity_id, OLD.contract_id, OLD.requisition_id,
      OLD.purchasing_org_id, OLD.document_number) THEN
    RAISE EXCEPTION 'purchase order % keeps its supplier, currency, legal entity, contract, requisition, purchasing org and number', OLD.document_number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_identity';
  END IF;
  IF NEW.applying_change_id IS NOT NULL AND NEW.applying_change_id IS DISTINCT FROM OLD.applying_change_id AND NOT EXISTS (
      SELECT FROM procurement.purchase_order_change WHERE tenant_id = NEW.tenant_id AND id = NEW.applying_change_id
        AND purchase_order_id = NEW.id AND status IN ('draft', 'pending_approval') AND base_revision = NEW.revision) THEN
    RAISE EXCEPTION 'purchase order % applies an open change of its own revision', OLD.document_number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_revision';
  END IF;
  IF (OLD.status <> 'draft' AND (NEW.payment_term_code, NEW.order_date, NEW.extensions) IS DISTINCT FROM (OLD.payment_term_code, OLD.order_date, OLD.extensions)
        AND OLD.applying_change_id IS NULL)
      OR (NEW.revision <> OLD.revision AND NOT (OLD.applying_change_id IS NOT NULL AND NEW.applying_change_id IS NULL))
      OR (OLD.applying_change_id IS NOT NULL AND NEW.applying_change_id IS NULL AND NEW.revision <> OLD.revision + 1) THEN
    RAISE EXCEPTION 'released purchase order % changes only through an order change', OLD.document_number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_revision';
  END IF;
  IF OLD.applying_change_id IS NOT NULL AND NEW.applying_change_id IS NULL AND NOT EXISTS (SELECT FROM procurement.purchase_order_change
      WHERE tenant_id = NEW.tenant_id AND id = OLD.applying_change_id AND status = 'applied' AND revision = NEW.revision) THEN
    RAISE EXCEPTION 'purchase order % clears the change it applied only once the change made its revision', OLD.document_number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_revision';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_order_revision() OWNER TO erp_owner;
CREATE TRIGGER purchase_order_revision BEFORE UPDATE ON procurement.purchase_order
  FOR EACH ROW EXECUTE FUNCTION procurement.check_order_revision();

-- A change is applied within one transaction: an order may not commit
-- naming the change it applies, so every application holds the order's
-- row lock from naming to clearing. A deferred check reads the order as
-- the transaction ends.
CREATE FUNCTION procurement.check_order_applied() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF EXISTS (SELECT FROM procurement.purchase_order WHERE tenant_id = NEW.tenant_id AND id = NEW.id AND applying_change_id IS NOT NULL) THEN
    RAISE EXCEPTION 'purchase order % applies a change within one transaction', NEW.document_number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_applied_at_once';
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION procurement.check_order_applied() OWNER TO erp_owner;
CREATE CONSTRAINT TRIGGER purchase_order_applied_at_once AFTER UPDATE OF applying_change_id ON procurement.purchase_order
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.applying_change_id IS NOT NULL)
  EXECUTE FUNCTION procurement.check_order_applied();

-- A draft's items change freely; an order waiting for approval's only by
-- being valued as it is released (000063); a released order's only while
-- a change is applied, keeping their account, unit, material and contract
-- item, and never below what was received.
CREATE OR REPLACE FUNCTION procurement.check_order_item_editable() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE st text; applying uuid; received numeric;
BEGIN
  SELECT status, applying_change_id INTO st, applying FROM procurement.purchase_order
  WHERE tenant_id = NEW.tenant_id AND id = NEW.purchase_order_id;
  IF st = 'draft' THEN
    RETURN NEW;
  END IF;
  IF TG_OP = 'UPDATE' AND st = 'pending_approval' AND OLD.valuation_price IS NULL AND NEW.valuation_price IS NOT NULL
     AND to_jsonb(NEW) - 'valuation_price' - 'valuation_price_unit' = to_jsonb(OLD) - 'valuation_price' - 'valuation_price_unit' THEN
    RETURN NEW;
  END IF;
  IF st <> 'released' OR applying IS NULL THEN
    RAISE EXCEPTION 'items of a % procurement.purchase_order cannot change', st USING ERRCODE = 'check_violation';
  END IF;
  IF NOT NEW.is_removed AND NEW.valuation_price IS NULL THEN
    RAISE EXCEPTION 'item % is added to released purchase order % with its valuation price', NEW.item_no, NEW.purchase_order_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_item_valued';
  END IF;
  IF TG_OP = 'UPDATE' AND (NEW.account_id, NEW.uom_code, NEW.material_id, NEW.contract_item_no)
     IS DISTINCT FROM (OLD.account_id, OLD.uom_code, OLD.material_id, OLD.contract_item_no) THEN
    RAISE EXCEPTION 'item % of released purchase order % keeps its account, unit, material and contract item', NEW.item_no, NEW.purchase_order_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_item_identity';
  END IF;
  SELECT coalesce(sum(quantity), 0) INTO received FROM procurement.goods_movement_item
  WHERE tenant_id = NEW.tenant_id AND purchase_order_id = NEW.purchase_order_id AND po_item_no = NEW.item_no;
  IF (NEW.is_removed AND received <> 0) OR NEW.quantity * (1 + NEW.over_delivery_pct / 100) < received THEN
    RAISE EXCEPTION 'item % of purchase order % would be below the % received', NEW.item_no, NEW.purchase_order_id, trim_scale(received)
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_item_received';
  END IF;
  RETURN NEW;
END $$;
DROP TRIGGER purchase_order_item_editable ON procurement.purchase_order_item;
CREATE TRIGGER purchase_order_item_editable BEFORE INSERT OR UPDATE ON procurement.purchase_order_item
  FOR EACH ROW EXECUTE FUNCTION procurement.check_order_item_editable();

SELECT platform.secure_table('procurement.purchase_order_change'::regclass);
