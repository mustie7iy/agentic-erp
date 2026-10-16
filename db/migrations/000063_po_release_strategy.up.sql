-- The release strategy of purchase orders (T47d, 03-data-model §3.8). An
-- order released without an approved requisition, or above its total,
-- waits in pending_approval for a purchase_order approval request, which
-- it names in release_request_id while it waits; its approval releases
-- it, its rejection or withdrawal returns it to draft. The statuses an
-- order moves through are kept here: draft -> pending_approval ->
-- released, back to draft, released -> closed, and cancelled from draft,
-- pending_approval or released. That the request was approved is checked
-- by procurement's approvals callback, which releases the order under the
-- guard release_request_id = the approved request: the rule stays within
-- procurement's schema (02-architecture, boundary rule 1).
ALTER TABLE procurement.purchase_order DROP CONSTRAINT purchase_order_status_check,
  ADD CONSTRAINT purchase_order_status_check CHECK (status IN ('draft', 'pending_approval', 'released', 'closed', 'cancelled')),
  ADD COLUMN release_request_id uuid, -- workflow context
  ADD CONSTRAINT purchase_order_release_request_pending CHECK ((status = 'pending_approval') = (release_request_id IS NOT NULL)),
  DROP CONSTRAINT purchase_order_draft_direct,
  ADD CONSTRAINT purchase_order_draft_direct CHECK (status NOT IN ('draft', 'pending_approval') OR valuation = 'direct');

CREATE OR REPLACE FUNCTION procurement.check_valuation_fixed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.valuation IS DISTINCT FROM OLD.valuation AND NOT (OLD.status IN ('draft', 'pending_approval') AND NEW.status = 'released') THEN
    RAISE EXCEPTION 'the valuation of purchase order % is fixed when it is released', OLD.document_number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_valuation_fixed';
  END IF;
  RETURN NEW;
END $$;

-- The items are valued as the order leaves draft, while they may still be
-- written: an order waiting for approval can change no item (000015).
CREATE OR REPLACE FUNCTION procurement.value_released_items() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'draft' AND NEW.status IN ('pending_approval', 'released') THEN
    UPDATE procurement.purchase_order_item SET valuation_price = unit_price, valuation_price_unit = price_unit
    WHERE tenant_id = NEW.tenant_id AND purchase_order_id = NEW.id AND NOT is_removed AND valuation_price IS NULL;
  END IF;
  RETURN NEW;
END $$;

CREATE FUNCTION procurement.check_order_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (OLD.status, NEW.status) NOT IN (('draft', 'pending_approval'), ('pending_approval', 'draft'), ('pending_approval', 'released'),
      ('draft', 'released'), ('released', 'closed'), ('draft', 'cancelled'), ('pending_approval', 'cancelled'), ('released', 'cancelled')) THEN
    RAISE EXCEPTION 'purchase order % cannot go from % to %', OLD.document_number, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'purchase_order_transition';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_order_transition() OWNER TO erp_owner;
CREATE TRIGGER purchase_order_transition BEFORE UPDATE OF status ON procurement.purchase_order
  FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status) EXECUTE FUNCTION procurement.check_order_transition();
