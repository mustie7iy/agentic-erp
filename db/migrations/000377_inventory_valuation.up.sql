-- Valuation and inventory postings (S1d; ADR-0082 §7-§8 and As built;
-- 03-data-model §3.35): a material's valuation in a plant, the valuation
-- area (SAP MBEW: price control, standard or moving price per price
-- unit, valuation class, total quantity and value), its price changes
-- (MR21) and the value each movement moves. A valuation's quantity,
-- value and moving price change only with its movements and price
-- changes (inventory.movement_stock, inventory.price_change_valuation),
-- and a standard-priced one's value is always its quantity at its price.
-- Every movement is valued from now on: inventory postings go through
-- the ledger (source context inventory) with the accounts of the
-- account determination's valuation classes. The account determination
-- check and the journal's source contexts are widened NOT VALID and
-- validated by 000378 (06 §6.4); the rest is expand only: tables and
-- columns the previous version neither reads nor writes, and a stricter
-- movement_stock that the previous version, which posts no valued
-- movement, never reaches.
ALTER TABLE ledger.account_determination DROP CONSTRAINT account_determination_key_check;
ALTER TABLE ledger.account_determination ADD CONSTRAINT account_determination_key_check
  CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'price_difference',
                 'cash_discount', 'fx_gain', 'fx_loss', 'rounding', 'down_payment', 'retained_earnings', 'contract_asset',
                 'contract_liability', 'right_of_use_asset', 'lease_liability', 'lease_interest', 'right_of_use_depreciation',
                 'right_of_use_accumulated_depreciation', 'payment_difference_expense', 'payment_difference_revenue')
         OR key ~ '^inventory_(stock|receipt|initial_entry|consumption|scrapping|price_difference|revaluation):[0-9A-Z]{1,4}$') NOT VALID;
ALTER TABLE ledger.journal_entry DROP CONSTRAINT journal_entry_source_context_check;
ALTER TABLE ledger.journal_entry ADD CONSTRAINT journal_entry_source_context_check
  CHECK (source_context IN ('ledger', 'payables', 'payments', 'procurement', 'receivables', 'assets', 'integration', 'controlling', 'revenue',
                            'leases', 'inventory')) NOT VALID;

-- A material's valuation in a plant: its price control, its price in the
-- company's currency per price unit of the base unit (the standard price,
-- or the moving price the movements give it), its valuation class, and
-- the quantity and value of its stock in the plant.
CREATE TABLE inventory.valuation (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,    -- tenancy context: the plant's company
  material_id      uuid NOT NULL,    -- materials context
  plant_id         uuid NOT NULL,    -- tenancy context
  valuation_class  text NOT NULL CONSTRAINT valuation_class_check CHECK (valuation_class ~ '^[0-9A-Z]{1,4}$'),
  price_control    text NOT NULL CONSTRAINT valuation_price_control_check CHECK (price_control IN ('standard', 'moving_average')),
  currency         char(3) NOT NULL REFERENCES ref.currency,
  price            bigint NOT NULL CONSTRAINT valuation_price_check CHECK (price >= 0),
  price_unit       integer NOT NULL CONSTRAINT valuation_price_unit_check CHECK (price_unit BETWEEN 1 AND 10000),
  quantity         numeric(20,6) NOT NULL DEFAULT 0 CONSTRAINT valuation_quantity_check CHECK (quantity >= 0),
  value            bigint NOT NULL DEFAULT 0 CONSTRAINT valuation_value_check CHECK (value >= 0),
  version          integer NOT NULL DEFAULT 1 CONSTRAINT valuation_version_check CHECK (version >= 1),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT valuation_once UNIQUE (tenant_id, material_id, plant_id),
  -- No stock, no value.
  CONSTRAINT valuation_value_without_stock CHECK (quantity > 0 OR value = 0),
  -- A standard-priced stock is worth its quantity at its standard price,
  -- rounded half away from zero, as an issue's value is taken from it.
  CONSTRAINT valuation_standard_value CHECK (price_control <> 'standard' OR value = round(quantity * price / price_unit))
);
CREATE TRIGGER valuation_touch BEFORE UPDATE ON inventory.valuation FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- A valuation starts with no stock; its price control, currency, price
-- unit and what it values stay as created.
CREATE FUNCTION inventory.valuation_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' AND (NEW.quantity <> 0 OR NEW.value <> 0) THEN
    RAISE EXCEPTION 'a valuation starts with no stock; movements add to it' USING ERRCODE = 'check_violation', CONSTRAINT = 'valuation_fixed';
  END IF;
  IF TG_OP = 'UPDATE' AND (NEW.material_id <> OLD.material_id OR NEW.plant_id <> OLD.plant_id OR NEW.legal_entity_id <> OLD.legal_entity_id
      OR NEW.price_control <> OLD.price_control OR NEW.currency <> OLD.currency OR NEW.price_unit <> OLD.price_unit) THEN
    RAISE EXCEPTION 'a valuation''s material, plant, company, price control, currency and price unit do not change'
      USING ERRCODE = 'check_violation', CONSTRAINT = 'valuation_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION inventory.valuation_fixed() OWNER TO erp_owner;
CREATE TRIGGER valuation_fixed BEFORE INSERT OR UPDATE ON inventory.valuation FOR EACH ROW EXECUTE FUNCTION inventory.valuation_fixed();
-- The list: the tenant's valuations in the order they were created.
CREATE INDEX valuation_by_created_at ON inventory.valuation (tenant_id, created_at, id);
SELECT platform.secure_table('inventory.valuation'::regclass);
-- The application changes a valuation's class, and nothing else of it;
-- movements and price changes change its quantity, value and price.
REVOKE UPDATE ON inventory.valuation FROM erp_app;
GRANT UPDATE (valuation_class, version, updated_by) ON inventory.valuation TO erp_app;

-- A price change (SAP MR21): the valuation's price before and after, the
-- stock it revalued and by how much, and the journal entry that posted
-- it, none when nothing was revalued. Insert-only; it sets the
-- valuation's price and adds the revaluation to its value
-- (inventory.price_change_valuation).
CREATE TABLE inventory.price_change (
  tenant_id             uuid NOT NULL,
  id                    uuid NOT NULL,
  valuation_id          uuid NOT NULL,
  old_price             bigint NOT NULL CONSTRAINT price_change_old_price_check CHECK (old_price >= 0),
  new_price             bigint NOT NULL CONSTRAINT price_change_new_price_check CHECK (new_price >= 0),
  quantity              numeric(20,6) NOT NULL CONSTRAINT price_change_quantity_check CHECK (quantity >= 0),
  amount                bigint NOT NULL,
  posting_date          date NOT NULL,
  journal_entry_id      uuid,             -- ledger context
  journal_entry_year    smallint,
  journal_entry_number  text,
  created_at            timestamptz NOT NULL DEFAULT now(),
  created_by            uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT price_change_valuation_fkey FOREIGN KEY (tenant_id, valuation_id) REFERENCES inventory.valuation (tenant_id, id),
  CONSTRAINT price_change_changes CHECK (new_price <> old_price),
  -- A revaluation is posted; no revaluation, no entry.
  CONSTRAINT price_change_entry CHECK ((journal_entry_id IS NULL) = (amount = 0)
    AND (journal_entry_id IS NULL) = (journal_entry_year IS NULL) AND (journal_entry_id IS NULL) = (journal_entry_number IS NULL))
);
CREATE TRIGGER price_change_insert_only BEFORE UPDATE OR DELETE ON inventory.price_change FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
-- A valuation's price changes, the latest first.
CREATE INDEX price_change_by_valuation ON inventory.price_change (tenant_id, valuation_id, created_at);
SELECT platform.secure_table('inventory.price_change'::regclass);
CREATE FUNCTION inventory.price_change_valuation() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  UPDATE inventory.valuation SET price = NEW.new_price, value = value + NEW.amount, version = version + 1, updated_by = NEW.created_by
  WHERE tenant_id = NEW.tenant_id AND id = NEW.valuation_id AND price = NEW.old_price AND quantity = NEW.quantity;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'a price change is of its valuation''s price and stock as they are' USING ERRCODE = 'check_violation', CONSTRAINT = 'price_change_valuation';
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION inventory.price_change_valuation() OWNER TO erp_owner;
CREATE TRIGGER price_change_valuation AFTER INSERT ON inventory.price_change FOR EACH ROW EXECUTE FUNCTION inventory.price_change_valuation();

-- What a movement moves in value (SAP MSEG's DMBTR): the stock's value
-- change, and what a standard-priced material's receipt or transfer is
-- valued at beyond its standard price, the price difference (PRD).
ALTER TABLE inventory.movement
  ADD COLUMN amount bigint NOT NULL DEFAULT 0,
  ADD COLUMN difference bigint NOT NULL DEFAULT 0;
-- The currency a material document's lines are valued in, its company's
-- (none on a document of S1c, before materials were valued), and the
-- journal entry it posted, none when it moved no value (a transfer
-- within a valuation area).
ALTER TABLE inventory.material_document
  ADD COLUMN currency char(3) REFERENCES ref.currency,
  ADD COLUMN journal_entry_id uuid,       -- ledger context
  ADD COLUMN journal_entry_year smallint,
  ADD COLUMN journal_entry_number text,
  ADD CONSTRAINT material_document_entry CHECK ((journal_entry_id IS NULL) = (journal_entry_year IS NULL)
    AND (journal_entry_id IS NULL) = (journal_entry_number IS NULL) AND (journal_entry_id IS NULL OR currency IS NOT NULL));

-- A movement adds its quantity to its stock row and its quantity and
-- value to its material's valuation in the plant, which must exist; a
-- moving-priced valuation's price follows, its value per price unit of
-- its quantity, rounded half away from zero while it has stock: exactly,
-- by integer division of numerics, as the application computes it (a
-- numeric quotient keeps only some sixteen digits).
CREATE OR REPLACE FUNCTION inventory.movement_stock() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  UPDATE inventory.stock SET quantity = quantity + NEW.quantity, updated_at = now()
  WHERE tenant_id = NEW.tenant_id AND material_id = NEW.material_id AND plant_id = NEW.plant_id AND storage_location_id = NEW.storage_location_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'a movement adds to a stock row written before it' USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'movement_stock';
  END IF;
  UPDATE inventory.valuation SET quantity = quantity + NEW.quantity, value = value + NEW.amount, version = version + 1,
    price = CASE WHEN price_control = 'moving_average' AND quantity + NEW.quantity > 0
      THEN div(2 * (value + NEW.amount)::numeric * price_unit + (quantity + NEW.quantity), 2 * (quantity + NEW.quantity))::bigint ELSE price END
  WHERE tenant_id = NEW.tenant_id AND material_id = NEW.material_id AND plant_id = NEW.plant_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'a movement is valued by its material''s valuation in the plant' USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'movement_valuation';
  END IF;
  RETURN NULL;
END $$;

-- Valuing materials and changing their prices (ADR-0082 §8).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('inventory.valuation.read', 'View the valuations of materials in plants and their price changes', false),
  ('inventory.valuation.write', 'Value materials in plants, change their valuation class and change their prices, which revalues their stock', false);
