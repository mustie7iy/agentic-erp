DROP TABLE IF EXISTS analytics.report, agents.inbox, agents.outbox, agents.recommendation, agents.tool_call, agents.session,
  integration.inbox, integration.outbox, integration.migration_run, integration.output_message, integration.message,
  integration.connector, integration.external_ref, audit.anchor, audit.chain_head, audit.audit_event CASCADE;
