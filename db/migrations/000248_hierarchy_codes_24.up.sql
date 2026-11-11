-- Hierarchy and node codes of up to 24 characters (the SAP specialist,
-- po:2026-11-09, A11(c) [M]): SETHEADER-SETNAME holds 24 characters, 15
-- being the limit of the CO transactions (KSH2, KCH2), so a set kept with a
-- longer name (GS02) migrates as a node, or as an alternative hierarchy
-- coded as its root set. 000139 held 15. Expand only: every code valid
-- before stays valid.

ALTER TABLE ledger.hierarchy DROP CONSTRAINT hierarchy_code_check,
  ADD CONSTRAINT hierarchy_code_check CHECK (code ~ '^[0-9A-Z_-]{1,24}$');
ALTER TABLE ledger.hierarchy_node DROP CONSTRAINT hierarchy_node_code_check,
  ADD CONSTRAINT hierarchy_node_code_check CHECK (code ~ '^[0-9A-Z_-]{1,24}$');
