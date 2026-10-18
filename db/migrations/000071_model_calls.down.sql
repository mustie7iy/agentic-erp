DELETE FROM ref.permission WHERE code = 'agents.model.invoke';
ALTER TABLE agents.session DROP COLUMN cost_micro;
DROP TABLE agents.model_call;
DROP TABLE ref.model_price;
