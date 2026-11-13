-- Dispute cases (T86a; ADR-0065 As built; 03-data-model §3.27): a
-- customer disputes an amount of one of its open items, a collections
-- specialist works the case and closes it as accepted or rejected. The
-- item is the ledger's, named by its journal line as a dunning notice's
-- item is; customer_id is the partner context's, legal_entity_id the
-- tenancy context's, assignee_id and closed_by the identity context's: no
-- foreign key leaves the schema (02 §2.1). Closing a case posts nothing.
-- Expand only (06 §6.4): a new table and two permissions, which the
-- previous version neither reads nor writes.

-- A dispute case. disputed_amount is minor units of currency, the item's,
-- more than zero; disputed_amount_lc its share of the item's amount in
-- currency_lc, the company's, as the item was booked. customer_number and
-- document_number are what the case was opened on, which the events name
-- the customer and the item by. A closed case has its outcome as status,
-- its reason and who closed it when; an open one has none of them.
CREATE TABLE collections.dispute_case (
  tenant_id           uuid NOT NULL,
  id                  uuid NOT NULL,
  legal_entity_id     uuid NOT NULL,     -- tenancy context
  customer_id         uuid NOT NULL,     -- partner context
  customer_number     text NOT NULL CONSTRAINT dispute_case_customer_number_check CHECK (length(customer_number) BETWEEN 1 AND 40),
  fiscal_year         smallint NOT NULL, -- ledger context: the journal line
  journal_entry_id    uuid NOT NULL,
  line_no             integer NOT NULL CONSTRAINT dispute_case_line_no_check CHECK (line_no > 0),
  document_number     text NOT NULL CONSTRAINT dispute_case_document_number_check CHECK (length(document_number) BETWEEN 1 AND 40),
  reference           text,
  currency            char(3) NOT NULL REFERENCES ref.currency,
  disputed_amount     bigint NOT NULL CONSTRAINT dispute_case_amount_check CHECK (disputed_amount > 0),
  currency_lc         char(3) NOT NULL REFERENCES ref.currency,
  disputed_amount_lc  bigint NOT NULL CONSTRAINT dispute_case_amount_lc_check CHECK (disputed_amount_lc >= 0),
  reason              text NOT NULL CONSTRAINT dispute_case_reason_check CHECK (btrim(reason) <> '' AND length(reason) <= 500),
  assignee_id         uuid,              -- identity context: a person
  note                text CONSTRAINT dispute_case_note_check CHECK (btrim(note) <> '' AND length(note) <= 2000),
  status              text NOT NULL CONSTRAINT dispute_case_status_check CHECK (status IN ('open', 'accepted', 'rejected')),
  close_reason        text CONSTRAINT dispute_case_close_reason_check CHECK (btrim(close_reason) <> '' AND length(close_reason) <= 500),
  closed_at           timestamptz,
  closed_by           uuid,              -- identity context
  version             integer NOT NULL DEFAULT 1 CONSTRAINT dispute_case_version_check CHECK (version >= 1),
  created_at          timestamptz NOT NULL DEFAULT now(),
  created_by          uuid NOT NULL,
  updated_at          timestamptz NOT NULL DEFAULT now(),
  updated_by          uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  -- An open case has no outcome; a closed one has its reason, time and person.
  CONSTRAINT dispute_case_closed_check CHECK ((status = 'open') = (close_reason IS NULL)
    AND (status = 'open') = (closed_at IS NULL) AND (status = 'open') = (closed_by IS NULL))
);
CREATE TRIGGER dispute_case_touch BEFORE UPDATE ON collections.dispute_case FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- An item has one open case at a time, whether the second comes after the
-- first or races it; a dunning run and the worklist read the open cases
-- of their items by it.
CREATE UNIQUE INDEX dispute_case_one_open ON collections.dispute_case (tenant_id, fiscal_year, journal_entry_id, line_no) WHERE status = 'open';
-- The list: the tenant's cases, the latest first, whichever companies the
-- caller may act in, in the order its pages are read.
CREATE INDEX dispute_case_by_created_at ON collections.dispute_case (tenant_id, created_at, id);
SELECT platform.secure_table('collections.dispute_case'::regclass);

-- An open case is assigned, noted on and closed, once; a closed case is
-- not changed. Its company, customer, item, amounts and reason stay (the
-- grant names the columns those changes set), and no case is deleted.
CREATE FUNCTION collections.check_dispute_case_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'open' THEN
    RAISE EXCEPTION 'dispute case %: a closed case is not changed', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'dispute_case_closed_once';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION collections.check_dispute_case_change() OWNER TO erp_owner;
CREATE TRIGGER dispute_case_closed_once BEFORE UPDATE ON collections.dispute_case
  FOR EACH ROW EXECUTE FUNCTION collections.check_dispute_case_change();
REVOKE UPDATE ON collections.dispute_case FROM erp_app;
GRANT UPDATE (assignee_id, note, status, close_reason, closed_at, closed_by, version, updated_by) ON collections.dispute_case TO erp_app;

-- Reading dispute cases, and opening, assigning and closing them (ADR-0065
-- As built, T86a).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('collections.dispute.read', 'View dispute cases of the companies one may act in', false),
  ('collections.dispute.write', 'Open, assign and close dispute cases', false);
