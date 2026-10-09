-- Workflow (03-data-model §3.11)

CREATE TABLE workflow.approval_policy (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  object_type      text NOT NULL CHECK (object_type IN ('requisition', 'partner_change', 'config_change', 'invoice_exception')),
  legal_entity_id  uuid,                         -- NULL: applies to every legal entity
  currency         char(3) REFERENCES ref.currency,
  valid_from       date NOT NULL,
  valid_to         date,
  config_version   integer NOT NULL,            -- configuration version that introduced it
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CHECK (valid_to IS NULL OR valid_to > valid_from),
  EXCLUDE USING gist (tenant_id WITH =, object_type WITH =, (coalesce(legal_entity_id, '00000000-0000-0000-0000-000000000000'::uuid)) WITH =,
                      daterange(valid_from, valid_to) WITH &&)
);

CREATE TABLE workflow.approval_tier (
  tenant_id     uuid NOT NULL,
  policy_id     uuid NOT NULL,
  tier_no       smallint NOT NULL CHECK (tier_no > 0),
  up_to_amount  bigint CHECK (up_to_amount >= 0),     -- NULL: no upper bound (last tier)
  PRIMARY KEY (tenant_id, policy_id, tier_no),
  FOREIGN KEY (tenant_id, policy_id) REFERENCES workflow.approval_policy (tenant_id, id)
);

CREATE TABLE workflow.approval_tier_step (
  tenant_id  uuid NOT NULL,
  policy_id  uuid NOT NULL,
  tier_no    smallint NOT NULL,
  step_no    smallint NOT NULL CHECK (step_no > 0),
  role_id    uuid NOT NULL,                       -- identity context
  required   smallint NOT NULL CHECK (required > 0),
  PRIMARY KEY (tenant_id, policy_id, tier_no, step_no),
  FOREIGN KEY (tenant_id, policy_id, tier_no) REFERENCES workflow.approval_tier (tenant_id, policy_id, tier_no)
);

CREATE TABLE workflow.approval_request (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,
  object_type   text NOT NULL,
  object_id     uuid NOT NULL,
  title         text NOT NULL,
  requester_id  uuid NOT NULL,
  amount        bigint,
  currency      char(3) REFERENCES ref.currency,
  policy_id     uuid,
  status        text NOT NULL CHECK (status IN ('pending', 'approved', 'rejected', 'cancelled')),
  current_step  smallint NOT NULL DEFAULT 1,
  completed_at  timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  version       integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, policy_id) REFERENCES workflow.approval_policy (tenant_id, id),
  CHECK ((status = 'pending') = (completed_at IS NULL)),
  CHECK ((amount IS NULL) = (currency IS NULL))
);
-- One open request per object at a time.
CREATE UNIQUE INDEX approval_request_one_pending ON workflow.approval_request (tenant_id, object_type, object_id) WHERE status = 'pending';
CREATE INDEX approval_request_by_status ON workflow.approval_request (tenant_id, status, created_at);
CREATE TRIGGER approval_request_touch BEFORE UPDATE ON workflow.approval_request FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE workflow.approval_step (
  tenant_id   uuid NOT NULL,
  request_id  uuid NOT NULL,
  step_no     smallint NOT NULL CHECK (step_no > 0),
  role_id     uuid NOT NULL,
  required    smallint NOT NULL CHECK (required > 0),
  PRIMARY KEY (tenant_id, request_id, step_no),
  FOREIGN KEY (tenant_id, request_id) REFERENCES workflow.approval_request (tenant_id, id)
);
CREATE INDEX approval_step_by_role ON workflow.approval_step (tenant_id, role_id);

CREATE TABLE workflow.decision (
  tenant_id   uuid NOT NULL,
  id          uuid NOT NULL,
  request_id  uuid NOT NULL,
  step_no     smallint NOT NULL,
  actor_id    uuid NOT NULL,
  actor_kind  text NOT NULL CHECK (actor_kind IN ('human', 'agent', 'service')),
  decision    text NOT NULL CHECK (decision IN ('approve', 'reject', 'cancel')),
  comment     text,
  decided_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, request_id, step_no) REFERENCES workflow.approval_step (tenant_id, request_id, step_no),
  -- Only people approve or reject (ADR-0010); an agent may withdraw a request it raised.
  CONSTRAINT decision_people_decide CHECK (actor_kind = 'human' OR decision = 'cancel'),
  CHECK (decision = 'approve' OR length(trim(coalesce(comment, ''))) > 0)      -- rejections and cancellations need a reason
);
-- A person approves a request at most once, across all its steps.
CREATE UNIQUE INDEX decision_one_approval_per_actor ON workflow.decision (tenant_id, request_id, actor_id) WHERE decision = 'approve';
CREATE TRIGGER decision_immutable BEFORE UPDATE OR DELETE ON workflow.decision FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- Requesters never decide their own request (except cancelling it), and only pending requests take decisions.
CREATE FUNCTION workflow.check_decision() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE r workflow.approval_request%ROWTYPE;
BEGIN
  SELECT * INTO r FROM workflow.approval_request WHERE tenant_id = NEW.tenant_id AND id = NEW.request_id FOR UPDATE;
  IF r.status <> 'pending' THEN
    RAISE EXCEPTION 'approval request is %', r.status USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.decision = 'cancel' AND NEW.actor_id <> r.requester_id THEN
    RAISE EXCEPTION 'only the requester can cancel' USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.decision <> 'cancel' AND NEW.actor_id = r.requester_id THEN
    RAISE EXCEPTION 'requesters cannot decide their own request' USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.decision <> 'cancel' AND NEW.step_no <> r.current_step THEN
    RAISE EXCEPTION 'decision is for step % but the request is at step %', NEW.step_no, r.current_step USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER decision_check BEFORE INSERT ON workflow.decision FOR EACH ROW EXECUTE FUNCTION workflow.check_decision();

SELECT platform.create_event_tables('workflow');
