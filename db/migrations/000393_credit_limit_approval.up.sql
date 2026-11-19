-- Approval of credit limit changes (T77d; ADR-0066 As built T77d;
-- 03-data-model §3.28): a new limit, or a change that raises a limit or
-- changes its validity, is a credit limit change approved through the
-- approvals context before it applies; a lowered limit or a new review
-- date applies at once, as before. A limit is `pending` until its create
-- change is approved, and not in effect while it is.
-- Expand only (06 §6.4): a column with a default, so every limit set
-- before, and every limit the previous version sets, is `active` as it
-- was; a table and a permission the previous version neither reads nor
-- writes.
ALTER TABLE credit.credit_limit
  ADD COLUMN status text NOT NULL DEFAULT 'active' CONSTRAINT credit_limit_status_check CHECK (status IN ('pending', 'active'));
-- Its approval makes a pending limit active; nothing makes an active one
-- pending again (credit_limit_status_guard).
GRANT UPDATE (status) ON credit.credit_limit TO erp_app;
CREATE FUNCTION credit.credit_limit_status_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'active' AND NEW.status <> 'active' THEN
    RAISE EXCEPTION 'credit_limit_status_guard: credit limit %: an active limit stays active', OLD.id USING ERRCODE = 'check_violation',
      CONSTRAINT = 'credit_limit_status_guard';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER credit_limit_status_guard BEFORE UPDATE OF status ON credit.credit_limit
  FOR EACH ROW EXECUTE FUNCTION credit.credit_limit_status_guard();

-- A credit limit change (ADR-0066 As built T77d): kind `create` for a
-- pending limit's first approval (asked for again after a rejection too),
-- `change` for a change of an active one; the limit's amount (in its
-- currency), validity and review date as asked for, and, for a change,
-- as they were when it was asked for. Decided once: `applied`,
-- `rejected` or `cancelled`, with decided_at. One pending per limit.
CREATE TABLE credit.credit_limit_change (
  tenant_id              uuid NOT NULL,
  id                     uuid NOT NULL,
  credit_limit_id        uuid NOT NULL,
  kind                   text NOT NULL CONSTRAINT credit_limit_change_kind_check CHECK (kind IN ('create', 'change')),
  amount                 bigint NOT NULL CONSTRAINT credit_limit_change_amount_check CHECK (amount >= 0),
  valid_from             date,
  valid_to               date,
  next_review_on         date,
  amount_before          bigint CONSTRAINT credit_limit_change_amount_before_check CHECK (amount_before >= 0),
  valid_from_before      date,
  valid_to_before        date,
  next_review_on_before  date,
  status                 text NOT NULL CONSTRAINT credit_limit_change_status_check CHECK (status IN ('pending', 'applied', 'rejected', 'cancelled')),
  workflow_request_id    uuid,              -- workflow context: its approval request
  requested_by           uuid NOT NULL,
  decided_at             timestamptz,
  version                integer NOT NULL DEFAULT 1 CONSTRAINT credit_limit_change_version_check CHECK (version >= 1),
  created_at             timestamptz NOT NULL DEFAULT now(),
  created_by             uuid NOT NULL,
  updated_at             timestamptz NOT NULL DEFAULT now(),
  updated_by             uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT credit_limit_change_limit_fkey FOREIGN KEY (tenant_id, credit_limit_id) REFERENCES credit.credit_limit (tenant_id, id),
  CONSTRAINT credit_limit_change_validity_check CHECK (valid_to >= valid_from),
  -- A create has no values before: its limit was never in effect.
  CONSTRAINT credit_limit_change_before_check CHECK ((kind = 'create') = (amount_before IS NULL)
    AND (kind = 'change' OR (valid_from_before IS NULL AND valid_to_before IS NULL AND next_review_on_before IS NULL))),
  CONSTRAINT credit_limit_change_decided_check CHECK ((status = 'pending') = (decided_at IS NULL))
);
-- One pending change per limit: a second waits for the first's decision.
CREATE UNIQUE INDEX credit_limit_change_one_pending ON credit.credit_limit_change (tenant_id, credit_limit_id) WHERE status = 'pending';
-- A limit's changes, newest first, and the tenant's.
CREATE INDEX credit_limit_change_by_limit ON credit.credit_limit_change (tenant_id, credit_limit_id, created_at, id);
CREATE INDEX credit_limit_change_by_created ON credit.credit_limit_change (tenant_id, created_at, id);
CREATE TRIGGER credit_limit_change_touch BEFORE UPDATE ON credit.credit_limit_change FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('credit.credit_limit_change'::regclass);
-- A change is linked to its request and decided once; what it asks for
-- stays, and no change is deleted.
REVOKE UPDATE ON credit.credit_limit_change FROM erp_app;
GRANT UPDATE (status, workflow_request_id, decided_at, version, updated_by) ON credit.credit_limit_change TO erp_app;
-- Decided once: a decided change stays as it was decided.
CREATE FUNCTION credit.credit_limit_change_decided_once() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'pending' THEN
    RAISE EXCEPTION 'credit_limit_change_decided_once: credit limit change %: it is %', OLD.id, OLD.status USING ERRCODE = 'check_violation',
      CONSTRAINT = 'credit_limit_change_decided_once';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER credit_limit_change_decided_once BEFORE UPDATE ON credit.credit_limit_change
  FOR EACH ROW EXECUTE FUNCTION credit.credit_limit_change_decided_once();

-- Approving credit limit changes: an approval permission, which agents
-- never hold (ADR-0066 §6, As built T77d).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('credit.limit.approve', 'Approve new and raised credit limits and changes of their validity', true);
