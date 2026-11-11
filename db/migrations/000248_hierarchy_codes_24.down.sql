-- Restores 000139's 15 characters; it fails while a code is longer.

ALTER TABLE ledger.hierarchy_node DROP CONSTRAINT hierarchy_node_code_check,
  ADD CONSTRAINT hierarchy_node_code_check CHECK (code ~ '^[0-9A-Z_-]{1,15}$');
ALTER TABLE ledger.hierarchy DROP CONSTRAINT hierarchy_code_check,
  ADD CONSTRAINT hierarchy_code_check CHECK (code ~ '^[0-9A-Z_-]{1,15}$');
