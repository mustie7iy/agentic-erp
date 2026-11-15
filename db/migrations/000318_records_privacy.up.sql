-- The personal data inventory (T100a; ADR-0079 §9-§10; 03-data-model
-- §3.32): reading which columns hold personal data and, for one business
-- partner or principal, how many rows of each name them. The inventory is
-- declared in internal/records, not stored, so this adds one permission
-- only. Expand only: the previous version does not know it (06 §6.4).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('records.privacy.read', 'View the personal data inventory and where a business partner''s or principal''s data is held', false);
