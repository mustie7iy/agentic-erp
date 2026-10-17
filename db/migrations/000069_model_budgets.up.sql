-- Model budgets (T42c, ADR-0036, 03-data-model §3.14): what each budget
-- line has spent this month, and what calls in flight have reserved
-- against it. A line is an agent's (agent_profile.monthly_budget) or the
-- tenant's (principal_id NULL; its cap is the AI policy's). The gateway
-- reserves a call's most it can cost on each of its lines before
-- forwarding it, and settles the actual cost after, both with the line
-- read FOR UPDATE (ADR-0008), so concurrent calls cannot overspend.
CREATE TABLE agents.spend (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,
  principal_id  uuid,                     -- the agent; NULL: the tenant's line
  month         date NOT NULL CONSTRAINT spend_month_check CHECK (month = date_trunc('month', month)::date),
  -- Caps and prices are in one currency (ADR-0036).
  currency      char(3) NOT NULL REFERENCES ref.currency CONSTRAINT spend_currency_check CHECK (currency = 'USD'),
  spent_micro   bigint NOT NULL DEFAULT 0 CONSTRAINT spend_spent_micro_check CHECK (spent_micro >= 0),
  -- The highest share of the cap announced (BudgetThresholdReached): each
  -- threshold is announced once a month.
  threshold     smallint NOT NULL DEFAULT 0 CONSTRAINT spend_threshold_check CHECK (threshold IN (0, 80, 100)),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  version       integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  -- No key to identity.principal: contexts do not reference each other's
  -- tables (03-data-model), as agents.model_call does not.
  CONSTRAINT spend_line_key UNIQUE NULLS NOT DISTINCT (tenant_id, month, principal_id)
);
CREATE TRIGGER spend_touch BEFORE UPDATE ON agents.spend FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('agents.spend'::regclass);

-- One row per call per line: the most the call may cost, held against
-- the line while the call is in flight. Settling the call marks it
-- settled. A call whose settlement was lost (the process stopped
-- mid-call) frees its line once the row expires, the call's time and its
-- margin after it was reserved, so no line is held for the rest of the
-- month by a call that will never settle.
CREATE TABLE agents.model_reservation (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,            -- the call's
  spend_id      uuid NOT NULL,
  amount_micro  bigint NOT NULL CONSTRAINT model_reservation_amount_check CHECK (amount_micro >= 0),
  reserved_at   timestamptz NOT NULL,
  expires_at    timestamptz NOT NULL,
  settled_at    timestamptz,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  version       integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id, spend_id),
  FOREIGN KEY (tenant_id, spend_id) REFERENCES agents.spend (tenant_id, id),
  CONSTRAINT model_reservation_expiry_check CHECK (expires_at > reserved_at)
);
-- What is held against a line: its unsettled reservations.
CREATE INDEX model_reservation_held ON agents.model_reservation (tenant_id, spend_id, expires_at) WHERE settled_at IS NULL;
CREATE TRIGGER model_reservation_touch BEFORE UPDATE ON agents.model_reservation FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('agents.model_reservation'::regclass);
