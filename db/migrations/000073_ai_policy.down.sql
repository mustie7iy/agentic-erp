DELETE FROM ref.permission WHERE code IN ('agents.ai_policy.read', 'agents.ai_policy.manage');
DROP TABLE agents.ai_policy;
