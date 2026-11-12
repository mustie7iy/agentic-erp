-- Catalog items (T84a; ADR-0071 §1-§3; plan-breadth-week2 §T84a,
-- 03-data-model §3.8): a supplier's item in a company's catalog, loaded
-- through the API, which a requester searches and (T84b) adds to a
-- requisition. The catalog is the tenant's own table: nothing is fetched
-- from a supplier's system. legal_entity_id is the tenancy context's and
-- supplier_id the partner context's: no foreign key leaves the schema
-- (02 §2.1). An item is active or retired; it is retired once and never
-- deleted, and what it offers is not changed (a new price is a new item
-- after the old one is retired). Expand only: a new table and two
-- permissions, which the previous version neither reads nor writes
-- (06 §6.4).
CREATE TABLE procurement.catalog_item (
  tenant_id         uuid NOT NULL,
  id                uuid NOT NULL,
  legal_entity_id   uuid NOT NULL,       -- tenancy context
  supplier_id       uuid NOT NULL,       -- partner context
  item_key          text NOT NULL CONSTRAINT catalog_item_item_key_check CHECK (length(item_key) BETWEEN 1 AND 40 AND item_key = btrim(item_key)),
  description       text NOT NULL CONSTRAINT catalog_item_description_check CHECK (length(btrim(description)) > 0 AND length(description) <= 200),
  uom_code          text NOT NULL CONSTRAINT catalog_item_uom_code_fkey REFERENCES ref.unit_of_measure,
  currency          char(3) NOT NULL CONSTRAINT catalog_item_currency_fkey REFERENCES ref.currency,
  unit_price        bigint NOT NULL CONSTRAINT catalog_item_unit_price_check CHECK (unit_price > 0),   -- minor units of currency, per price_unit
  price_unit        integer NOT NULL DEFAULT 1 CONSTRAINT catalog_item_price_unit_check CHECK (price_unit > 0),
  -- The contract item that prices an order of it, if any: an item of a
  -- contract of the same company and supplier (checked when it is loaded).
  contract_id       uuid,
  contract_item_no  integer,
  status            text NOT NULL DEFAULT 'active' CONSTRAINT catalog_item_status_check CHECK (status IN ('active', 'retired')),
  -- What the search reads: the key and the description in lower case.
  search_text       text GENERATED ALWAYS AS (lower(item_key || ' ' || description)) STORED,
  version           integer NOT NULL DEFAULT 1,
  created_at        timestamptz NOT NULL DEFAULT now(),
  created_by        uuid NOT NULL,
  updated_at        timestamptz NOT NULL DEFAULT now(),
  updated_by        uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT catalog_item_contract_item_both CHECK ((contract_id IS NULL) = (contract_item_no IS NULL)),
  CONSTRAINT catalog_item_contract_item_fkey FOREIGN KEY (tenant_id, contract_id, contract_item_no)
    REFERENCES procurement.contract_item (tenant_id, contract_id, item_no)
);
CREATE TRIGGER catalog_item_touch BEFORE UPDATE ON procurement.catalog_item FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- An item key is the supplier's once among a company's active items; a
-- retired item frees its key.
CREATE UNIQUE INDEX catalog_item_active_key ON procurement.catalog_item (tenant_id, legal_entity_id, supplier_id, item_key) WHERE status = 'active';
-- The search: a company's items in the order its pages are read.
CREATE INDEX catalog_item_by_key ON procurement.catalog_item (tenant_id, legal_entity_id, item_key, id);
SELECT platform.secure_table('procurement.catalog_item'::regclass);
-- Retiring is the only change: the application may set the status and
-- what moves with it, nothing an item offers.
REVOKE UPDATE ON procurement.catalog_item FROM erp_app;
GRANT UPDATE (status, version, updated_by, updated_at) ON procurement.catalog_item TO erp_app;

-- Searching the catalog and loading it (ADR-0071 §5).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('procurement.catalog.read', 'Search and view the catalog items of the companies one may act in', false),
  ('procurement.catalog.manage', 'Load and retire catalog items', false);
