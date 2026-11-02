-- The liquidity forecast (T53e; ADR-0051 §9, §15; 03-data-model §3.18):
-- its read permission. The forecast is computed on read from supplier open
-- items and payment runs; nothing is stored. Expand only: a new
-- permission, which no role holds until the configuration grants it.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('cash.forecast.read', 'View the liquidity forecast of a company', false);
