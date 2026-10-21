-- The cash context (T53b; ADR-0051 §2, §4; 03-data-model §3.18): a leaf
-- that reads the ledger, payments, payables and procurement through their
-- published ports and owns liquidity items, memo records, snapshots and
-- variance comments in its own schema. This step keeps the configuration's
-- liquidity items and planning groups (cash.liquidity_items,
-- cash.planning_groups), kept by cash.Setup; the derivation rules and the
-- companies' settings are read from the active document. Expand only: a
-- new schema, which the previous version neither reads nor writes.
CREATE SCHEMA cash;
GRANT USAGE ON SCHEMA cash TO erp_app, erp_readonly, erp_owner;

-- A liquidity item (SAP FLQLITEM): cash by purpose, in one tree. Items the
-- configuration no longer has become inactive, never deleted, since memo
-- records and snapshots name them; the configuration's validation keeps
-- the tree acyclic, at most four levels deep, and directions fitting.
CREATE TABLE cash.liquidity_item (
  tenant_id    uuid NOT NULL,
  code         text NOT NULL CHECK (code ~ '^[A-Z][A-Z0-9_]{0,29}$'),
  name         text NOT NULL CHECK (btrim(name) <> ''),
  direction    text NOT NULL CHECK (direction IN ('inflow', 'outflow', 'both')),
  parent_code  text CHECK (parent_code <> code),
  is_active    boolean NOT NULL DEFAULT true,  -- false once the configuration no longer has it
  version      integer NOT NULL DEFAULT 1,
  created_at   timestamptz NOT NULL DEFAULT now(),
  created_by   uuid NOT NULL,
  updated_at   timestamptz NOT NULL DEFAULT now(),
  updated_by   uuid NOT NULL,
  PRIMARY KEY (tenant_id, code),
  FOREIGN KEY (tenant_id, parent_code) REFERENCES cash.liquidity_item (tenant_id, code)
);
CREATE TRIGGER liquidity_item_touch BEFORE UPDATE ON cash.liquidity_item FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('cash.liquidity_item'::regclass);

-- A planning group (SAP T035, LFB1-FDGRV): suppliers classified by how
-- they are paid. A supplier's company data names one
-- (partner.supplier_company.planning_group, checked through
-- cash.PlanningGroups); the partner context refuses a configuration that
-- takes away a group a supplier has (partner.PlanningGroupCodes).
CREATE TABLE cash.planning_group (
  tenant_id    uuid NOT NULL,
  code         text NOT NULL CHECK (code ~ '^[A-Z][A-Z0-9_]{0,29}$'),
  name         text NOT NULL CHECK (btrim(name) <> ''),
  is_active    boolean NOT NULL DEFAULT true,  -- false once the configuration no longer has it
  version      integer NOT NULL DEFAULT 1,
  created_at   timestamptz NOT NULL DEFAULT now(),
  created_by   uuid NOT NULL,
  updated_at   timestamptz NOT NULL DEFAULT now(),
  updated_by   uuid NOT NULL,
  PRIMARY KEY (tenant_id, code)
);
CREATE TRIGGER planning_group_touch BEFORE UPDATE ON cash.planning_group FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('cash.planning_group'::regclass);
