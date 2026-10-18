-- Autonomy levels are enforced from T42g on (ADR-0036): level 0 reads,
-- 1 also proposes, 2 writes what the agent's permissions allow. The agents
-- that exist were given their permissions to write (the invoice
-- exceptions agent, the delivery agents), so they are raised to level 2
-- (PO, 2026-10-18); agents created from now on start at 0 and an
-- administrator raises them.
-- Neither audited nor published: no agent does anything else for it (the
-- names already routed to these models; levels were not enforced before).
UPDATE identity.agent_profile SET autonomy_level = 2, updated_at = now(), version = version + 1 WHERE autonomy_level < 2;
