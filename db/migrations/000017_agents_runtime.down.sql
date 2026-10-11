DROP INDEX agents.recommendation_by_object;
DELETE FROM ref.permission WHERE code IN ('agents.session.read', 'agents.session.record', 'agents.recommendation.decide');
