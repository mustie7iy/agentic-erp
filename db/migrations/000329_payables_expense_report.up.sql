-- Expense reports (T87a; ADR-0074; 03-data-model §3.9): an accountant
-- enters an employee's expenses, each line with its date, expense type,
-- gross amount in the company's currency, input tax code and cost
-- center, and posts the report once. The employee is a business partner
-- with the supplier role (human capital is outside the required scope),
-- so the posting's credit is the employee's open item, which a payment
-- run pays. A report is entered, then posted; it is not changed or
-- cancelled in the first version.
-- Expand only (06 §6.4): two tables and three permissions, which the
-- previous version neither reads nor writes.
CREATE TABLE payables.expense_report (
  tenant_id           uuid NOT NULL,
  id                  uuid NOT NULL,
  document_number     text NOT NULL CHECK (length(document_number) BETWEEN 1 AND 40),  -- the expense_report number range
  legal_entity_id     uuid NOT NULL,              -- tenancy context
  employee_id         uuid NOT NULL,              -- partner context: a business partner with the supplier role
  report_date         date NOT NULL,              -- the company's date when entered: the document and tax date
  currency            char(3) NOT NULL REFERENCES ref.currency,  -- the company's
  description         text CONSTRAINT expense_report_description_check CHECK (btrim(description) <> '' AND length(description) <= 200),
  status              text NOT NULL DEFAULT 'entered' CONSTRAINT expense_report_status_check CHECK (status IN ('entered', 'posted')),
  posting_date        date,
  journal_entry_year  smallint,
  journal_entry_id    uuid,                       -- ledger context
  payment_term_code   text,
  net_due_date        date,
  version             integer NOT NULL DEFAULT 1,
  created_at          timestamptz NOT NULL DEFAULT now(),
  created_by          uuid NOT NULL,
  updated_at          timestamptz NOT NULL DEFAULT now(),
  updated_by          uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT expense_report_number_once UNIQUE (tenant_id, legal_entity_id, document_number),
  -- A posted report names its entry, its posting date and its open item's terms; an entered one none of them.
  CONSTRAINT expense_report_posted CHECK ((status = 'posted') = (journal_entry_id IS NOT NULL)
    AND (journal_entry_id IS NULL) = (journal_entry_year IS NULL) AND (journal_entry_id IS NULL) = (posting_date IS NULL)
    AND (journal_entry_id IS NULL) = (payment_term_code IS NULL) AND (journal_entry_id IS NULL) = (net_due_date IS NULL))
);
CREATE TRIGGER expense_report_touch BEFORE UPDATE ON payables.expense_report FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list: a company's reports by date, an employee's.
CREATE INDEX expense_report_by_date ON payables.expense_report (tenant_id, legal_entity_id, report_date, id);
CREATE INDEX expense_report_by_employee ON payables.expense_report (tenant_id, employee_id, report_date, id);
SELECT platform.secure_table('payables.expense_report'::regclass);

-- A report is entered first and is posted once; posting is its only
-- change (the grant names the columns posting sets; nothing is deleted).
-- The application reads the report FOR UPDATE before it posts it
-- (ADR-0008).
CREATE FUNCTION payables.check_expense_report_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'entered' THEN
      RAISE EXCEPTION 'an expense report is entered first, not %', NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'expense_report_posted_once';
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.status <> 'entered' OR NEW.status <> 'posted' THEN
    RAISE EXCEPTION 'expense report %: only an entered report changes, and only by being posted', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'expense_report_posted_once';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION payables.check_expense_report_change() OWNER TO erp_owner;
CREATE TRIGGER expense_report_posted_once BEFORE INSERT OR UPDATE ON payables.expense_report
  FOR EACH ROW EXECUTE FUNCTION payables.check_expense_report_change();
REVOKE UPDATE ON payables.expense_report FROM erp_app;
GRANT UPDATE (status, posting_date, journal_entry_year, journal_entry_id, payment_term_code, net_due_date, version, updated_by)
  ON payables.expense_report TO erp_app;

-- A line of an expense report: an expense of one day and type, its gross
-- amount in the report's currency with the input tax it holds by its tax
-- code at the report date, on the type's expense account as it was when
-- the report was entered, and its cost center. Lines are written with
-- their report and never changed or deleted (the table has no version,
-- so the application is granted neither; 000011).
CREATE TABLE payables.expense_report_line (
  tenant_id          uuid NOT NULL,
  expense_report_id  uuid NOT NULL,
  line_no            integer NOT NULL CHECK (line_no BETWEEN 1 AND 999),
  expense_date       date NOT NULL,
  expense_type       text NOT NULL CONSTRAINT expense_report_line_type_check CHECK (expense_type ~ '^[A-Z0-9_]{1,20}$'),
  account_id         uuid NOT NULL,              -- ledger context
  amount             bigint NOT NULL CONSTRAINT expense_report_line_amount_positive CHECK (amount > 0),  -- gross
  tax_code           text,
  tax_amount         bigint NOT NULL DEFAULT 0,
  cost_center_id     uuid,                       -- ledger context; none: the account's default
  description        text CONSTRAINT expense_report_line_description_check CHECK (btrim(description) <> '' AND length(description) <= 200),
  PRIMARY KEY (tenant_id, expense_report_id, line_no),
  -- The tax a gross holds is less than the gross, and none without a code.
  CONSTRAINT expense_report_line_tax CHECK (CASE WHEN tax_code IS NULL THEN tax_amount = 0 ELSE tax_amount >= 0 AND tax_amount < amount END),
  CONSTRAINT expense_report_line_report_fkey FOREIGN KEY (tenant_id, expense_report_id) REFERENCES payables.expense_report (tenant_id, id)
);
SELECT platform.secure_table('payables.expense_report_line'::regclass);

-- Reading expense reports, entering them and posting them (ADR-0074 §5);
-- none is an approval permission.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('payables.expense.read', 'View expense reports of the companies one may act in', false),
  ('payables.expense.write', 'Enter expense reports of employees', false),
  ('payables.expense.post', 'Post expense reports', false);
