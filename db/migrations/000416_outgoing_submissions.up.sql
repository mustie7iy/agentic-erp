-- Sending outgoing e-invoices (E3; ADR-0061 §3, §9, As built E3;
-- 03-data-model §3.25): a valid outgoing e-invoice given to the company's
-- accredited provider (Evatra) through an einvoice_provider connector.
-- The submission is the outbox of its transmission (ADR-0007): written in
-- the request's transaction with the Operation that carries it out, before
-- any call, and sent after that commit by the send workflow, which then
-- polls the provider's status. One submission per e-invoice: a failed one
-- is queued again in its row by a person's new send. connector_id is the
-- integration context's, legal_entity_id the tenancy context's and
-- operation_id the platform's: no foreign key leaves the schema but the
-- one to the e-invoice (02 §2.1). Expand only: a new table and a
-- permission, which the previous version neither reads nor writes.
CREATE TABLE einvoicing.outgoing_submission (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  outgoing_einvoice_id uuid NOT NULL,
  legal_entity_id      uuid NOT NULL,        -- tenancy context: the seller
  connector_id         uuid NOT NULL,        -- integration context: the provider's connector
  operation_id         uuid NOT NULL,        -- the Operation of the send asked for last
  filename             text NOT NULL CHECK (length(filename) BETWEEN 1 AND 40),  -- the e-invoice's number (IBT-001), the provider's filename
  business_type        text NOT NULL CHECK (business_type IN ('B2B', 'B2G')),
  status               text NOT NULL CHECK (status IN ('queued', 'sending', 'submitted', 'completed', 'failed')),
  provider_status      text CHECK (provider_status ~ '^[A-Z_]{1,40}$'),            -- the provider's processing status, as it last reported it
  tracking_id          text CHECK (length(tracking_id) BETWEEN 1 AND 200),        -- the provider's id of the current submission
  failed_tracking_ids  text[] NOT NULL DEFAULT '{}',                               -- earlier submissions the provider failed to send (SENDING_FAILED)
  attempts             smallint NOT NULL DEFAULT 0 CHECK (attempts >= 0),          -- calls made for the current send
  resends              smallint NOT NULL DEFAULT 0 CHECK (resends >= 0),           -- sends again after SENDING_FAILED
  polls                integer NOT NULL DEFAULT 0 CHECK (polls >= 0),              -- status reads of the current submission
  look_up              boolean NOT NULL DEFAULT false,                             -- a call was made before: look the document up before sending
  next_check_at        timestamptz,                                                -- when the workflow next sends or reads the status
  problem_code         text CHECK (problem_code ~ '^[a-z]+\.[a-z0-9_.]+$'),
  problem_detail       text CHECK (length(problem_detail) BETWEEN 1 AND 1000),
  submitted_at         timestamptz,
  completed_at         timestamptz,
  requested_by         uuid NOT NULL,
  version              integer NOT NULL DEFAULT 1,
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT outgoing_submission_once UNIQUE (tenant_id, outgoing_einvoice_id),
  CONSTRAINT outgoing_submission_einvoice FOREIGN KEY (tenant_id, outgoing_einvoice_id) REFERENCES einvoicing.outgoing_einvoice (tenant_id, id),
  -- Submitted and completed carry the provider's tracking id.
  CONSTRAINT outgoing_submission_tracked CHECK (status NOT IN ('submitted', 'completed') OR tracking_id IS NOT NULL),
  -- A submission in progress has its next check; a finished one none.
  CONSTRAINT outgoing_submission_due CHECK ((status IN ('queued', 'sending', 'submitted')) = (next_check_at IS NOT NULL)),
  -- A failed one says why, and only a failed one.
  CONSTRAINT outgoing_submission_problem CHECK ((status = 'failed') = (problem_code IS NOT NULL) AND (problem_code IS NULL) = (problem_detail IS NULL)),
  CONSTRAINT outgoing_submission_completed CHECK ((status = 'completed') = (completed_at IS NOT NULL))
);
CREATE TRIGGER outgoing_submission_touch BEFORE UPDATE ON einvoicing.outgoing_submission FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list's filter by transmission status.
CREATE INDEX outgoing_submission_by_status ON einvoicing.outgoing_submission (tenant_id, status, outgoing_einvoice_id);
SELECT platform.secure_table('einvoicing.outgoing_submission'::regclass);

-- The transmission's moves (ADR-0061 As built E3): queued to sending (a
-- call claimed) or failed (refused before a call); sending back to queued
-- (the call failed, tried again), to itself (a claim that lapsed, its
-- outcome unknown, claimed again), to submitted (the provider's tracking
-- id) or failed; submitted to itself (a status read), queued (sent again
-- after SENDING_FAILED), completed or failed; failed to queued (a person
-- sends it again). Completed is final. What names the e-invoice is fixed.
CREATE FUNCTION einvoicing.check_outgoing_submission() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (OLD.status, NEW.status) NOT IN (('queued', 'sending'), ('queued', 'failed'), ('sending', 'queued'), ('sending', 'sending'), ('sending', 'submitted'),
      ('sending', 'failed'), ('submitted', 'submitted'), ('submitted', 'queued'), ('submitted', 'completed'), ('submitted', 'failed'),
      ('failed', 'queued')) THEN
    RAISE EXCEPTION 'an e-invoice submission does not move from % to %', OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'outgoing_submission_transition';
  END IF;
  IF (NEW.tenant_id, NEW.id, NEW.outgoing_einvoice_id, NEW.legal_entity_id, NEW.filename, NEW.created_at, NEW.created_by)
      IS DISTINCT FROM (OLD.tenant_id, OLD.id, OLD.outgoing_einvoice_id, OLD.legal_entity_id, OLD.filename, OLD.created_at, OLD.created_by) THEN
    RAISE EXCEPTION 'e-invoice submission % keeps its e-invoice', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'outgoing_submission_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION einvoicing.check_outgoing_submission() OWNER TO erp_owner;
CREATE TRIGGER outgoing_submission_transition BEFORE UPDATE ON einvoicing.outgoing_submission FOR EACH ROW
  EXECUTE FUNCTION einvoicing.check_outgoing_submission();

-- Sending outgoing e-invoices (ADR-0061 §11, As built E3), given to no
-- role, as E2's are.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('einvoicing.outgoing.send', 'Send valid e-invoices of the companies one may act in to their accredited e-invoicing provider', false);
