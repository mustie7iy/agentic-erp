-- Procurement (03-data-model §3.8)

CREATE TABLE procurement.requisition (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  document_number      text NOT NULL,
  legal_entity_id      uuid NOT NULL,           -- tenancy context
  requester_id         uuid NOT NULL,           -- identity context
  supplier_id          uuid,                    -- partner context (suggested)
  contract_id          uuid,
  currency             char(3) NOT NULL REFERENCES ref.currency,
  needed_by            date,
  status               text NOT NULL CHECK (status IN ('draft', 'pending_approval', 'approved', 'rejected', 'cancelled', 'ordered')),
  workflow_request_id  uuid,                    -- workflow context
  extensions           jsonb NOT NULL DEFAULT '{}',
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid NOT NULL,
  version              integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, legal_entity_id, document_number)
);
CREATE INDEX requisition_by_status ON procurement.requisition (tenant_id, legal_entity_id, status);
CREATE INDEX requisition_by_requester ON procurement.requisition (tenant_id, requester_id, created_at);
CREATE TRIGGER requisition_touch BEFORE UPDATE ON procurement.requisition FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE procurement.requisition_item (
  tenant_id       uuid NOT NULL,
  requisition_id  uuid NOT NULL,
  item_no         integer NOT NULL CHECK (item_no > 0),
  material_id     uuid,                          -- product context (later)
  description     text NOT NULL CHECK (length(trim(description)) > 0),
  quantity        numeric(20,6) NOT NULL CHECK (quantity > 0),
  uom_code        text NOT NULL REFERENCES ref.unit_of_measure,
  unit_price      bigint NOT NULL CHECK (unit_price >= 0),   -- estimate, requisition currency
  price_unit      integer NOT NULL DEFAULT 1 CHECK (price_unit > 0),
  account_id      uuid NOT NULL,                 -- ledger context
  PRIMARY KEY (tenant_id, requisition_id, item_no),
  FOREIGN KEY (tenant_id, requisition_id) REFERENCES procurement.requisition (tenant_id, id)
);

CREATE TABLE procurement.contract (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  document_number  text NOT NULL,
  legal_entity_id  uuid NOT NULL,
  supplier_id      uuid NOT NULL,
  currency         char(3) NOT NULL REFERENCES ref.currency,
  valid_from       date NOT NULL,
  valid_to         date NOT NULL,
  target_value     bigint NOT NULL CHECK (target_value > 0),
  status           text NOT NULL CHECK (status IN ('draft', 'active', 'expired', 'terminated', 'cancelled')),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  version          integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, legal_entity_id, document_number),
  CHECK (valid_to >= valid_from)
);
CREATE INDEX contract_by_supplier ON procurement.contract (tenant_id, supplier_id, status);
CREATE TRIGGER contract_touch BEFORE UPDATE ON procurement.contract FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE procurement.contract_item (
  tenant_id    uuid NOT NULL,
  contract_id  uuid NOT NULL,
  item_no      integer NOT NULL CHECK (item_no > 0),
  material_id  uuid,
  material_key text NOT NULL,                      -- supplier or internal material number
  description  text NOT NULL,
  unit_price   bigint NOT NULL CHECK (unit_price > 0),
  price_unit   integer NOT NULL DEFAULT 1 CHECK (price_unit > 0),
  uom_code     text NOT NULL REFERENCES ref.unit_of_measure,
  account_id   uuid NOT NULL,
  PRIMARY KEY (tenant_id, contract_id, item_no),
  UNIQUE (tenant_id, contract_id, material_key),
  FOREIGN KEY (tenant_id, contract_id) REFERENCES procurement.contract (tenant_id, id)
);

CREATE TABLE procurement.purchase_order (
  tenant_id          uuid NOT NULL,
  id                 uuid NOT NULL,
  document_number    text NOT NULL,
  legal_entity_id    uuid NOT NULL,
  purchasing_org_id  uuid,                        -- tenancy context
  supplier_id        uuid NOT NULL,
  currency           char(3) NOT NULL REFERENCES ref.currency,
  order_date         date NOT NULL,
  payment_term_code  text NOT NULL,              -- partner context
  requisition_id     uuid,
  contract_id        uuid,
  status             text NOT NULL CHECK (status IN ('draft', 'released', 'closed', 'cancelled')),
  extensions         jsonb NOT NULL DEFAULT '{}',
  created_at         timestamptz NOT NULL DEFAULT now(),
  created_by         uuid NOT NULL,
  updated_at         timestamptz NOT NULL DEFAULT now(),
  updated_by         uuid NOT NULL,
  version            integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, legal_entity_id, document_number),
  FOREIGN KEY (tenant_id, requisition_id) REFERENCES procurement.requisition (tenant_id, id),
  FOREIGN KEY (tenant_id, contract_id) REFERENCES procurement.contract (tenant_id, id)
);
CREATE UNIQUE INDEX purchase_order_one_per_requisition ON procurement.purchase_order (tenant_id, requisition_id)
  WHERE requisition_id IS NOT NULL AND status <> 'cancelled';
CREATE INDEX purchase_order_by_supplier ON procurement.purchase_order (tenant_id, supplier_id, order_date);
CREATE TRIGGER purchase_order_touch BEFORE UPDATE ON procurement.purchase_order FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE procurement.purchase_order_item (
  tenant_id          uuid NOT NULL,
  purchase_order_id  uuid NOT NULL,
  item_no            integer NOT NULL CHECK (item_no > 0),
  material_id        uuid,
  description        text NOT NULL,
  quantity           numeric(20,6) NOT NULL CHECK (quantity > 0),
  uom_code           text NOT NULL REFERENCES ref.unit_of_measure,
  unit_price         bigint NOT NULL CHECK (unit_price >= 0),
  price_unit         integer NOT NULL DEFAULT 1 CHECK (price_unit > 0),
  account_id         uuid NOT NULL,
  over_delivery_pct  numeric(9,6) NOT NULL DEFAULT 0 CHECK (over_delivery_pct BETWEEN 0 AND 100),
  tax_code           text,
  contract_item_no   integer,
  PRIMARY KEY (tenant_id, purchase_order_id, item_no),
  FOREIGN KEY (tenant_id, purchase_order_id) REFERENCES procurement.purchase_order (tenant_id, id)
);

-- Contract consumption as insert-only releases; the consumed value is their sum.
CREATE TABLE procurement.contract_release (
  tenant_id          uuid NOT NULL,
  contract_id        uuid NOT NULL,
  purchase_order_id  uuid NOT NULL,
  value              bigint NOT NULL CHECK (value <> 0),   -- negative when an order is cancelled
  released_at        timestamptz NOT NULL DEFAULT now(),
  released_by        uuid NOT NULL,
  PRIMARY KEY (tenant_id, contract_id, purchase_order_id, released_at),
  FOREIGN KEY (tenant_id, contract_id) REFERENCES procurement.contract (tenant_id, id),
  FOREIGN KEY (tenant_id, purchase_order_id) REFERENCES procurement.purchase_order (tenant_id, id)
);
CREATE TRIGGER contract_release_immutable BEFORE UPDATE OR DELETE ON procurement.contract_release FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- Goods movements: insert-only; 102 reverses a 101.
CREATE TABLE procurement.goods_movement (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  document_number  text NOT NULL,
  legal_entity_id  uuid NOT NULL,
  plant_id         uuid,
  movement_type    text NOT NULL CHECK (movement_type IN ('101', '102')),
  document_date    date NOT NULL,
  posting_date     date NOT NULL,
  reference        text,
  reversal_of_id   uuid,
  posted_at        timestamptz NOT NULL DEFAULT now(),
  posted_by        uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, legal_entity_id, document_number),
  FOREIGN KEY (tenant_id, reversal_of_id) REFERENCES procurement.goods_movement (tenant_id, id),
  CHECK ((movement_type = '102') = (reversal_of_id IS NOT NULL))
);
CREATE TABLE procurement.goods_movement_item (
  tenant_id          uuid NOT NULL,
  goods_movement_id  uuid NOT NULL,
  item_no            integer NOT NULL CHECK (item_no > 0),
  purchase_order_id  uuid NOT NULL,
  po_item_no         integer NOT NULL,
  quantity           numeric(20,6) NOT NULL CHECK (quantity <> 0),
  uom_code           text NOT NULL REFERENCES ref.unit_of_measure,
  PRIMARY KEY (tenant_id, goods_movement_id, item_no),
  FOREIGN KEY (tenant_id, goods_movement_id) REFERENCES procurement.goods_movement (tenant_id, id),
  FOREIGN KEY (tenant_id, purchase_order_id, po_item_no) REFERENCES procurement.purchase_order_item (tenant_id, purchase_order_id, item_no)
);
CREATE INDEX goods_movement_item_by_po ON procurement.goods_movement_item (tenant_id, purchase_order_id, po_item_no);
CREATE TRIGGER goods_movement_immutable BEFORE UPDATE OR DELETE ON procurement.goods_movement FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER goods_movement_item_immutable BEFORE UPDATE OR DELETE ON procurement.goods_movement_item FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- Received quantity per PO item, never below zero and never above the over-delivery tolerance.
-- The PO item row lock serializes concurrent receipts on one item. The
-- application role may not update PO items, so the function runs as the
-- table owner (000011); row-level security still applies to that owner.
CREATE FUNCTION procurement.check_receipt() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE ordered numeric; tol numeric; received numeric;
BEGIN
  SELECT quantity, over_delivery_pct INTO ordered, tol FROM procurement.purchase_order_item
  WHERE tenant_id = NEW.tenant_id AND purchase_order_id = NEW.purchase_order_id AND item_no = NEW.po_item_no FOR UPDATE;
  SELECT coalesce(sum(quantity), 0) INTO received FROM procurement.goods_movement_item
  WHERE tenant_id = NEW.tenant_id AND purchase_order_id = NEW.purchase_order_id AND po_item_no = NEW.po_item_no;
  IF received < 0 THEN
    RAISE EXCEPTION 'reversal exceeds the quantity received on PO item %', NEW.po_item_no USING ERRCODE = 'check_violation';
  END IF;
  IF received > ordered * (1 + tol / 100) THEN
    RAISE EXCEPTION 'receipt of % exceeds ordered % plus %%% tolerance', received, ordered, tol USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;
CREATE TRIGGER goods_movement_item_check AFTER INSERT ON procurement.goods_movement_item FOR EACH ROW EXECUTE FUNCTION procurement.check_receipt();

SELECT platform.create_event_tables('procurement');
