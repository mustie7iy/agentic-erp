-- Records management (T99a; ADR-0079 §1-§4; 03-data-model §3.32): reading
-- which fiscal years of a company's documents are past retention. The
-- retention rules are a section of the configuration document
-- (companies[].retention[]), so the context has no table yet: its schema
-- comes with the archiving runs (T99b). Expand only: one permission,
-- which the previous version does not know (06 §6.4).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('records.retention.read', 'View which fiscal years of a company''s documents are past retention', false);
