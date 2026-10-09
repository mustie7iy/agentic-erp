DROP TABLE IF EXISTS workflow.inbox, workflow.outbox, workflow.decision, workflow.approval_step, workflow.approval_request,
  workflow.approval_tier_step, workflow.approval_tier, workflow.approval_policy CASCADE;
DROP FUNCTION IF EXISTS workflow.check_decision();
