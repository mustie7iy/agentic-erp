-- Runtime support for agents on the API (T6): agents record their sessions,
-- tool calls and recommendations through the API; people decide the
-- recommendations.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('agents.session.read', 'View agent sessions, tool calls and recommendations', false),
  ('agents.session.record', 'Record agent sessions, tool calls and recommendations', false),
  ('agents.recommendation.decide', 'Decide agent recommendations', true);

CREATE INDEX recommendation_by_object ON agents.recommendation (tenant_id, object_type, object_id);
