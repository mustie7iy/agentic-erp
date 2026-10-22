-- A supplier's planning group in a legal entity (T53b; ADR-0051 §4; SAP
-- LFB1-FDGRV): a code of the configuration's cash.planning_groups, which
-- the cash context keeps (cash.planning_group; no foreign key crosses the
-- contexts, 03-data-model §3.1), giving the supplier's flows a liquidity
-- item when no account rule does. Nullable without a default, so adding it
-- rewrites nothing; the check is added NOT VALID and validated by 000143
-- (06 §6.4). The previous version neither reads nor writes it.
ALTER TABLE partner.supplier_company ADD COLUMN planning_group text;
ALTER TABLE partner.supplier_company ADD CONSTRAINT supplier_company_planning_group
  CHECK (planning_group ~ '^[A-Z][A-Z0-9_]{0,29}$') NOT VALID;
