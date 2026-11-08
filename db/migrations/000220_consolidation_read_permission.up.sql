-- The consolidation read permission (T68a; ADR-0059 §12): the groups and
-- the group chart now, and the runs and their figures as later slices add
-- them. The group chart, the mapping and the groups are configuration, so
-- this slice adds no table. Expand only: a new permission, which no role
-- holds until it is granted.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('consolidation.read', 'View consolidation groups, the group chart and consolidation runs', false);
