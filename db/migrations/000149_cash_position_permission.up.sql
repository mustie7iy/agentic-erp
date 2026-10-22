-- The cash position (T53c; ADR-0051 §8, §15; 03-data-model §3.18): its
-- read permission. The position is computed on read from statements,
-- payments and the ledger; nothing is stored. Expand only: a new
-- permission, which no role holds until the configuration grants it.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('cash.position.read', 'View the daily cash position of house banks', false);
