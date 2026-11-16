-- Process mining (T103a; ADR-0080): the processmining context computes
-- event logs on read from the documents' links and the audit trail and
-- stores nothing, so it has no schema; reading a log needs a permission
-- of its own. Expand only (06 §6.4).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('processmining.log.read', 'Read the event logs of business processes', false);
