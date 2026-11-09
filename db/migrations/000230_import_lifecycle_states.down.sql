-- The trigger functions as 000015, 000052, 000194, 000196, 000224 and
-- 000227 left them: every insert starts in its first state.
CREATE OR REPLACE FUNCTION platform.check_editable_parent() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE st text;
BEGIN
  -- TG_ARGV: parent table, column referring to it, editable statuses...
  EXECUTE format('SELECT status FROM %s WHERE tenant_id = $1 AND id = $2', TG_ARGV[0])
    INTO st USING NEW.tenant_id, (to_jsonb(NEW) ->> TG_ARGV[1])::uuid;
  IF NOT st = ANY (TG_ARGV[2:]) THEN
    RAISE EXCEPTION 'items of a % % cannot change', st, TG_ARGV[0] USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION ledger.check_parked_entry_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'parked' THEN
      RAISE EXCEPTION 'an entry is parked first, not %', NEW.status USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF (OLD.status, NEW.status) NOT IN (('parked', 'parked'), ('parked', 'pending_approval'), ('parked', 'posted'), ('parked', 'cancelled'),
      ('pending_approval', 'posted'), ('pending_approval', 'rejected'), ('pending_approval', 'parked')) THEN
    RAISE EXCEPTION 'parked entry % cannot go from % to %', OLD.id, OLD.status, NEW.status USING ERRCODE = 'check_violation';
  END IF;
  IF (NEW.id, NEW.created_by_kind, NEW.created_by, NEW.created_at) IS DISTINCT FROM (OLD.id, OLD.created_by_kind, OLD.created_by, OLD.created_at) THEN
    RAISE EXCEPTION 'who parked entry %, and when, does not change', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF (OLD.status, NEW.status) <> ('parked', 'parked') AND
     (NEW.legal_entity_id, NEW.ledger_id, NEW.document_type, NEW.document_date, NEW.posting_date, NEW.currency, NEW.reference, NEW.header_text)
     IS DISTINCT FROM
     (OLD.legal_entity_id, OLD.ledger_id, OLD.document_type, OLD.document_date, OLD.posting_date, OLD.currency, OLD.reference, OLD.header_text) THEN
    RAISE EXCEPTION 'parked entry % changes only while it is parked', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF (OLD.status, NEW.status) = ('parked', 'posted') AND NEW.approval_request_id IS NOT NULL THEN
    RAISE EXCEPTION 'parked entry % posted directly names no approval request', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.status = 'pending_approval' AND NEW.status <> 'parked' AND NEW.approval_request_id IS DISTINCT FROM OLD.approval_request_id THEN
    RAISE EXCEPTION 'parked entry % is decided only on the request it names', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.owner_id IS NOT NULL AND NEW.owner_id IS DISTINCT FROM OLD.owner_id THEN
    RAISE EXCEPTION 'the owner of parked entry % does not change', OLD.id USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.journal_entry_id IS NOT NULL AND NOT EXISTS (SELECT FROM ledger.journal_entry j
      WHERE j.tenant_id = NEW.tenant_id AND j.fiscal_year = NEW.journal_fiscal_year AND j.id = NEW.journal_entry_id
        AND j.source_context = 'ledger' AND j.source_document_id = NEW.id AND j.reversal_of_id IS NULL
        AND (j.legal_entity_id, j.ledger_id, j.document_type, j.document_date, j.posting_date, j.currency_tc)
          = (NEW.legal_entity_id, NEW.ledger_id, NEW.document_type, NEW.document_date, NEW.posting_date, NEW.currency)) THEN
    RAISE EXCEPTION 'journal entry % was not posted for parked entry %', NEW.journal_entry_id, OLD.id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION closing.check_period_close() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  changing text[] := ARRAY['status', 'version', 'updated_at', 'updated_by'];
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'open' THEN
      RAISE EXCEPTION 'a period close starts open, not %', NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'period_close_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF to_jsonb(NEW) - changing IS DISTINCT FROM to_jsonb(OLD) - changing THEN
    RAISE EXCEPTION 'period close % changes only its status', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'period_close_fixed';
  END IF;
  RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION closing.check_close_task() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  changing text[] := ARRAY['assignee_id', 'status', 'completed_by', 'completed_at', 'note', 'version', 'updated_at', 'updated_by',
    'checked_at', 'checked_by', 'check_passed', 'check_result'];
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'open' THEN
      RAISE EXCEPTION 'a close task starts open, not %', NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'close_task_transition';
    END IF;
    IF NEW.checked_at IS NOT NULL THEN
      RAISE EXCEPTION 'a close task starts unchecked'
        USING ERRCODE = 'check_violation', CONSTRAINT = 'close_task_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF to_jsonb(NEW) - changing IS DISTINCT FROM to_jsonb(OLD) - changing THEN
    RAISE EXCEPTION 'close task % of close % changes only its assignee, status and check result', OLD.code, OLD.close_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'close_task_fixed';
  END IF;
  -- A completion is recorded by completing the task and cleared by
  -- reopening it, never rewritten while it stands.
  IF NEW.status = OLD.status AND (NEW.completed_by, NEW.completed_at, NEW.note) IS DISTINCT FROM (OLD.completed_by, OLD.completed_at, OLD.note) THEN
    RAISE EXCEPTION 'close task % of close % keeps its completion while it is %', OLD.code, OLD.close_id, OLD.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'close_task_transition';
  END IF;
  -- A check result is replaced by the next check, never removed.
  IF OLD.checked_at IS NOT NULL AND NEW.checked_at IS NULL THEN
    RAISE EXCEPTION 'close task % of close % keeps its check result', OLD.code, OLD.close_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'close_task_transition';
  END IF;
  RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION consolidation.check_run() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'queued' THEN
      RAISE EXCEPTION 'a consolidation run starts queued, not %', NEW.status USING ERRCODE = 'check_violation', CONSTRAINT = 'run_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.status IN ('superseded', 'discarded') OR (NEW.status <> OLD.status AND (OLD.status, NEW.status) NOT IN
      (('queued', 'running'), ('running', 'computed'), ('queued', 'failed'), ('running', 'failed'), ('computed', 'final'),
       ('computed', 'discarded'), ('failed', 'discarded'), ('final', 'superseded'))) THEN
    RAISE EXCEPTION 'consolidation run % cannot change from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'run_transition';
  END IF;
  IF (NEW.group_code, NEW.parent_id, NEW.companies, NEW.fiscal_year, NEW.fiscal_period, NEW.period_start, NEW.period_end, NEW.config_version,
      NEW.group_currency, NEW.rate_type, NEW.operation_id, NEW.created_at, NEW.created_by) IS DISTINCT FROM (OLD.group_code, OLD.parent_id,
      OLD.companies, OLD.fiscal_year, OLD.fiscal_period, OLD.period_start, OLD.period_end, OLD.config_version, OLD.group_currency, OLD.rate_type,
      OLD.operation_id, OLD.created_at, OLD.created_by)
      OR (OLD.problem IS NOT NULL AND NEW.problem IS DISTINCT FROM OLD.problem)
      OR (OLD.computed_at IS NOT NULL AND NEW.computed_at IS DISTINCT FROM OLD.computed_at)
      OR (OLD.finalized_at IS NOT NULL AND (NEW.finalized_at, NEW.finalized_by) IS DISTINCT FROM (OLD.finalized_at, OLD.finalized_by)) THEN
    RAISE EXCEPTION 'consolidation run % keeps what it consolidates and its outcome', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'run_fixed';
  END IF;
  RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION consolidation.check_run_rows() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  st text;
BEGIN
  SELECT status INTO st FROM consolidation.run WHERE tenant_id = NEW.tenant_id AND id = NEW.run_id;
  IF st IS DISTINCT FROM 'running' THEN
    RAISE EXCEPTION 'consolidation run % is %, not running: its rows are written while it runs', NEW.run_id, st
      USING ERRCODE = 'check_violation', CONSTRAINT = 'run_rows_while_running';
  END IF;
  RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION ledger.check_carry_forward_run() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'carry-forward run % is kept', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'carry_forward_run_fixed';
  END IF;
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'current' THEN
      RAISE EXCEPTION 'a carry-forward run is current first, not %', NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'carry_forward_run_lifecycle';
    END IF;
    IF NEW.chained_from_id IS NOT NULL AND NOT EXISTS (SELECT FROM ledger.carry_forward_run r WHERE r.tenant_id = NEW.tenant_id
        AND r.id = NEW.chained_from_id AND r.legal_entity_id = NEW.legal_entity_id AND r.ledger_id = NEW.ledger_id
        AND r.to_fiscal_year = NEW.from_fiscal_year) THEN
      RAISE EXCEPTION 'carry-forward run % chains only from the previous year''s run of its company and ledger', NEW.id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'carry_forward_run_chain';
    END IF;
    IF NOT EXISTS (SELECT FROM ledger.gl_account a WHERE a.tenant_id = NEW.tenant_id AND a.id = NEW.retained_earnings_account_id
        AND a.account_type = 'equity') THEN
      RAISE EXCEPTION 'carry-forward run % carries the result to an equity account', NEW.id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'carry_forward_run_retained_earnings';
    END IF;
    RETURN NEW;
  END IF;
  IF NOT (OLD.status = 'current' AND NEW.status = 'superseded') THEN
    RAISE EXCEPTION 'carry-forward run % cannot go from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'carry_forward_run_lifecycle';
  END IF;
  IF (NEW.legal_entity_id, NEW.ledger_id, NEW.from_fiscal_year, NEW.to_fiscal_year, NEW.basis, NEW.chained_from_id,
      NEW.retained_earnings_account_id, NEW.row_count, NEW.created_at, NEW.created_by)
     IS DISTINCT FROM
     (OLD.legal_entity_id, OLD.ledger_id, OLD.from_fiscal_year, OLD.to_fiscal_year, OLD.basis, OLD.chained_from_id,
      OLD.retained_earnings_account_id, OLD.row_count, OLD.created_at, OLD.created_by) THEN
    RAISE EXCEPTION 'carry-forward run % is fixed but for being superseded', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'carry_forward_run_fixed';
  END IF;
  RETURN NEW;
END $$;
DROP FUNCTION platform.importing(uuid);
