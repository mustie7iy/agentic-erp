-- The financial close's read permission (T64a; ADR-0058 §3, §13): the
-- close calendar now, and the closes, their tasks, accrual proposals and
-- variances as later slices add them. The close template is configuration,
-- so this slice adds no table. Expand only: a new permission, which no
-- role holds until it is granted.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('closing.close.read', 'View the close calendar and period closes of a company', false);
