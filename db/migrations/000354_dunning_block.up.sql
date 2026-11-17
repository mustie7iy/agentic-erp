-- Dunning blocks (T76e; ADR-0065 As built; 03-data-model §3.27): a credit
-- controller blocks a customer, or one of its open items, from dunning in
-- a company, with a reason and an end date, and may release the block
-- early. A run started while a block is in force leaves the blocked items
-- out of its notices for a key date up to the end date, and names them
-- on the run (blocked_items). customer_id is the partner context's,
-- legal_entity_id the tenancy context's, the item's journal line the
-- ledger's and released_by the identity context's: no foreign key leaves
-- the schema (02 §2.1). Nothing is posted.
-- Expand only (06 §6.4): a new table, a column with a default and a
-- permission, which the previous version neither reads nor writes.

-- A block of the customer (no item) or of one of its open items (the
-- journal line, with its document number when blocked). customer_number
-- is the customer's when blocked. valid_to is the last key date the block
-- holds for; a released block holds for none.
CREATE TABLE collections.dunning_block (
  tenant_id         uuid NOT NULL,
  id                uuid NOT NULL,
  legal_entity_id   uuid NOT NULL,     -- tenancy context
  customer_id       uuid NOT NULL,     -- partner context
  customer_number   text NOT NULL CONSTRAINT dunning_block_customer_number_check CHECK (length(customer_number) BETWEEN 1 AND 40),
  fiscal_year       smallint,          -- ledger context: the journal line
  journal_entry_id  uuid,
  line_no           integer CONSTRAINT dunning_block_line_no_check CHECK (line_no > 0),
  document_number   text CONSTRAINT dunning_block_document_number_check CHECK (length(document_number) BETWEEN 1 AND 40),
  reason            text NOT NULL CONSTRAINT dunning_block_reason_check CHECK (btrim(reason) <> '' AND length(reason) <= 500),
  valid_to          date NOT NULL,
  released_at       timestamptz,
  released_by       uuid,              -- identity context
  version           integer NOT NULL DEFAULT 1 CONSTRAINT dunning_block_version_check CHECK (version >= 1),
  created_at        timestamptz NOT NULL DEFAULT now(),
  created_by        uuid NOT NULL,
  updated_at        timestamptz NOT NULL DEFAULT now(),
  updated_by        uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  -- An item block names the whole journal line and its document number; a customer block none of them.
  CONSTRAINT dunning_block_item_check CHECK ((fiscal_year IS NULL) = (journal_entry_id IS NULL) AND (fiscal_year IS NULL) = (line_no IS NULL)
    AND (fiscal_year IS NULL) = (document_number IS NULL)),
  -- A released block has when and by whom.
  CONSTRAINT dunning_block_released_check CHECK ((released_at IS NULL) = (released_by IS NULL))
);
CREATE TRIGGER dunning_block_touch BEFORE UPDATE ON collections.dunning_block FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The blocks a run reads: a company's unreleased blocks, by end date.
CREATE INDEX dunning_block_in_force ON collections.dunning_block (tenant_id, legal_entity_id, valid_to) WHERE released_at IS NULL;
-- The list: the tenant's blocks, the latest first, in the order its pages are read.
CREATE INDEX dunning_block_by_created_at ON collections.dunning_block (tenant_id, created_at, id);
SELECT platform.secure_table('collections.dunning_block'::regclass);

-- A block is released once; a released block is not changed. Its company,
-- customer, item, reason and end date stay (the grant names the columns a
-- release sets), and no block is deleted.
CREATE FUNCTION collections.check_dunning_block_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.released_at IS NOT NULL THEN
    RAISE EXCEPTION 'dunning block %: a released block is not changed', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'dunning_block_released_once';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION collections.check_dunning_block_change() OWNER TO erp_owner;
CREATE TRIGGER dunning_block_released_once BEFORE UPDATE ON collections.dunning_block
  FOR EACH ROW EXECUTE FUNCTION collections.check_dunning_block_change();
REVOKE UPDATE ON collections.dunning_block FROM erp_app;
GRANT UPDATE (released_at, released_by, version, updated_by) ON collections.dunning_block TO erp_app;

-- The items a run left out for a dunning block, a list written once with
-- the run, as its disputed items are (the update grant leaves it out).
ALTER TABLE collections.dunning_run
  ADD COLUMN blocked_items jsonb NOT NULL DEFAULT '[]'
    CONSTRAINT dunning_run_blocked_items_check CHECK (jsonb_typeof(blocked_items) = 'array');

-- Placing and releasing dunning blocks (ADR-0065 As built, T76e); they are
-- read with collections.dunning.read.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('collections.dunning.block', 'Block customers and their items from dunning, and release the blocks', false);
