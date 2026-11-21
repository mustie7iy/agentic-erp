-- Delivery status and FTA reporting of outgoing e-invoices (E4; ADR-0061
-- §9, As built E4; 03-data-model §3.25): once the accredited provider
-- reports a submission COMPLETED, the send goes on reading what Peppol
-- and the authority report back of it, the message level response of the
-- buyer's access point (Evatra: ACCEPTANCE, ACKNOWLEDGING, REJECTION,
-- BUSINESS_RULE_FATAL, BUSINESS_RULE_WARNING, DELIVERY_FAILURE,
-- SYNTAX_VIOLATION) and the tax data document's status (TDD_SEND,
-- TDD_FAILED, TDD_APPROVED, TDD_ACKNOWLEDGED, TDD_REJECTED, WITHDRAW_SEND,
-- WITHDRAW_FAILED), and a person may ask for the tax data document to be
-- sent again (retry) or withdrawn, the request the outbox of the call the
-- send's step makes after that commit. Expand only: new nullable or
-- defaulted columns, which the previous version neither reads nor writes,
-- and a trigger that allows what it allowed.
ALTER TABLE einvoicing.outgoing_submission
  ADD COLUMN delivery_status    text CHECK (delivery_status ~ '^[A-Z_]{1,40}$'),     -- the buyer's access point's message level response, as last reported
  ADD COLUMN delivery_detail    text CHECK (length(delivery_detail) BETWEEN 1 AND 1000), -- what the response says of it
  ADD COLUMN delivered_at       timestamptz,                                          -- when the response was recorded
  ADD COLUMN tax_data_status    text CHECK (tax_data_status ~ '^[A-Z_]{1,40}$'),     -- the tax data document's status, as last reported
  ADD COLUMN tax_data_at        timestamptz,                                          -- when that status was recorded
  ADD COLUMN tax_data_request   text CHECK (tax_data_request IN ('retry', 'withdraw')), -- a person's request, not yet made at the provider
  ADD COLUMN tax_data_attempts  smallint NOT NULL DEFAULT 0 CHECK (tax_data_attempts >= 0), -- calls made for the request
  ADD COLUMN tax_data_awaited   text CHECK (tax_data_awaited ~ '^[A-Z_]{1,40}$'),   -- the status a request made left: followed until it moves
  ADD COLUMN follow_polls       integer NOT NULL DEFAULT 0 CHECK (follow_polls >= 0), -- reads of the delivery status since completion or the last request
  ADD COLUMN follow_check_at    timestamptz,                                          -- when the delivery status is next read; null once final
  -- Only a completed submission is followed and carries what was reported.
  ADD CONSTRAINT outgoing_submission_followed CHECK (status = 'completed' OR (follow_check_at IS NULL AND delivery_status IS NULL
    AND tax_data_status IS NULL AND tax_data_awaited IS NULL)),
  -- A request is made by a step, so it is due.
  ADD CONSTRAINT outgoing_submission_request_due CHECK (tax_data_request IS NULL OR follow_check_at IS NOT NULL),
  ADD CONSTRAINT outgoing_submission_delivery CHECK ((delivery_status IS NULL) = (delivered_at IS NULL)
    AND (delivery_detail IS NULL OR delivery_status IS NOT NULL)),
  ADD CONSTRAINT outgoing_submission_tax_data CHECK ((tax_data_status IS NULL) = (tax_data_at IS NULL));
-- The sends due for a delivery read, which a cell without a workflow
-- engine lists.
CREATE INDEX outgoing_submission_followed_due ON einvoicing.outgoing_submission (tenant_id, follow_check_at) WHERE follow_check_at IS NOT NULL;
-- The in-process sender finds, across tenants, those with a delivery read
-- or a request due, whose send's Operation has finished (the worker
-- role, as the e-invoice fetcher's; the read itself runs in the tenant's
-- scope).
GRANT SELECT (tenant_id, follow_check_at) ON einvoicing.outgoing_submission TO erp_worker;
-- A credit note naming the invoice it corrects (IBT-025), which takes a
-- rejected e-invoice off the worklist once transmitted.
CREATE INDEX outgoing_einvoice_by_reference ON einvoicing.outgoing_einvoice (tenant_id, legal_entity_id, reference_invoice_number)
  WHERE reference_invoice_number IS NOT NULL;

-- The transmission's moves (ADR-0061 As built E3, E4): as before, and a
-- completed submission to itself (its delivery read, a person's request
-- of the tax data document and its outcome), in which what the provider
-- reported of the transmission stays as it was.
CREATE OR REPLACE FUNCTION einvoicing.check_outgoing_submission() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (OLD.status, NEW.status) NOT IN (('queued', 'sending'), ('queued', 'failed'), ('sending', 'queued'), ('sending', 'sending'), ('sending', 'submitted'),
      ('sending', 'failed'), ('submitted', 'submitted'), ('submitted', 'queued'), ('submitted', 'completed'), ('submitted', 'failed'),
      ('failed', 'queued'), ('completed', 'completed')) THEN
    RAISE EXCEPTION 'an e-invoice submission does not move from % to %', OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'outgoing_submission_transition';
  END IF;
  IF (NEW.tenant_id, NEW.id, NEW.outgoing_einvoice_id, NEW.legal_entity_id, NEW.filename, NEW.created_at, NEW.created_by)
      IS DISTINCT FROM (OLD.tenant_id, OLD.id, OLD.outgoing_einvoice_id, OLD.legal_entity_id, OLD.filename, OLD.created_at, OLD.created_by) THEN
    RAISE EXCEPTION 'e-invoice submission % keeps its e-invoice', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'outgoing_submission_fixed';
  END IF;
  IF OLD.status = 'completed' AND (NEW.connector_id, NEW.business_type, NEW.provider_status, NEW.tracking_id, NEW.completed_at, NEW.submitted_at)
      IS DISTINCT FROM (OLD.connector_id, OLD.business_type, OLD.provider_status, OLD.tracking_id, OLD.completed_at, OLD.submitted_at) THEN
    RAISE EXCEPTION 'completed e-invoice submission % keeps its transmission', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'outgoing_submission_completed_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION einvoicing.check_outgoing_submission() OWNER TO erp_owner;
