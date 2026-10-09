-- General ledger (03-data-model §3.7, ADR-0005, ADR-0006)

CREATE TABLE ledger.ledger (
  tenant_id             uuid NOT NULL,
  id                    uuid NOT NULL,
  code                  text NOT NULL CHECK (code ~ '^[0-9A-Z]{2}$'),     -- e.g. 0L, 2L
  name                  text NOT NULL,
  is_leading            boolean NOT NULL,
  accounting_principle  text NOT NULL CHECK (accounting_principle ~ '^[A-Z0-9_]{2,20}$'),  -- IFRS, US_GAAP, HGB
  created_at            timestamptz NOT NULL DEFAULT now(),
  created_by            uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, code)
);
CREATE UNIQUE INDEX ledger_one_leading ON ledger.ledger (tenant_id) WHERE is_leading;

CREATE TABLE ledger.fiscal_calendar (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  code             text NOT NULL CHECK (code ~ '^[A-Z0-9]{1,4}$'),        -- e.g. K4
  name             text NOT NULL,
  normal_periods   smallint NOT NULL CHECK (normal_periods BETWEEN 1 AND 16),
  special_periods  smallint NOT NULL DEFAULT 0 CHECK (special_periods BETWEEN 0 AND 4),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, code),
  CHECK (normal_periods + special_periods <= 16)
);

CREATE TABLE ledger.fiscal_period (
  tenant_id    uuid NOT NULL,
  calendar_id  uuid NOT NULL,
  fiscal_year  smallint NOT NULL CHECK (fiscal_year BETWEEN 1900 AND 2999),
  period       smallint NOT NULL CHECK (period BETWEEN 1 AND 16),
  start_date   date NOT NULL,
  end_date     date NOT NULL,
  is_special   boolean NOT NULL DEFAULT false,      -- year-end adjustment periods share the last period's dates
  PRIMARY KEY (tenant_id, calendar_id, fiscal_year, period),
  FOREIGN KEY (tenant_id, calendar_id) REFERENCES ledger.fiscal_calendar (tenant_id, id),
  CHECK (end_date >= start_date)
);
-- Normal periods of a calendar never overlap.
CREATE INDEX fiscal_period_dates ON ledger.fiscal_period (tenant_id, calendar_id, start_date, end_date);
ALTER TABLE ledger.fiscal_period ADD CONSTRAINT fiscal_period_no_overlap
  EXCLUDE USING gist (tenant_id WITH =, calendar_id WITH =, daterange(start_date, end_date, '[]') WITH &&) WHERE (NOT is_special);

CREATE TABLE ledger.period_status (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,                  -- API identity of the posting period of one entity and ledger
  legal_entity_id  uuid NOT NULL,                  -- tenancy context
  ledger_id        uuid NOT NULL,
  calendar_id      uuid NOT NULL,
  fiscal_year      smallint NOT NULL,
  period           smallint NOT NULL,
  status           text NOT NULL CHECK (status IN ('open', 'closed', 'locked')),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  version          integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, legal_entity_id, ledger_id, fiscal_year, period),
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, ledger_id) REFERENCES ledger.ledger (tenant_id, id),
  FOREIGN KEY (tenant_id, calendar_id, fiscal_year, period) REFERENCES ledger.fiscal_period (tenant_id, calendar_id, fiscal_year, period)
);
CREATE TRIGGER period_status_touch BEFORE UPDATE ON ledger.period_status FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE ledger.chart_of_accounts (
  tenant_id   uuid NOT NULL,
  id          uuid NOT NULL,
  code        text NOT NULL CHECK (code ~ '^[A-Z0-9]{1,4}$'),
  name        text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  created_by  uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, code)
);

CREATE TABLE ledger.gl_account (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  chart_id             uuid NOT NULL,
  number               text NOT NULL CHECK (number ~ '^[0-9A-Z]{1,10}$'),
  name                 text NOT NULL,
  account_type         text NOT NULL CHECK (account_type IN ('asset', 'liability', 'equity', 'revenue', 'expense')),
  is_balance_sheet     boolean GENERATED ALWAYS AS (account_type IN ('asset', 'liability', 'equity')) STORED,
  reconciliation_kind  text NOT NULL DEFAULT 'none' CHECK (reconciliation_kind IN ('none', 'supplier', 'customer', 'asset')),
  status               text NOT NULL CHECK (status IN ('active', 'blocked', 'inactive')),
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid NOT NULL,
  version              integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, chart_id, number),
  FOREIGN KEY (tenant_id, chart_id) REFERENCES ledger.chart_of_accounts (tenant_id, id),
  CHECK (reconciliation_kind = 'none' OR account_type IN ('asset', 'liability'))
);
CREATE TRIGGER gl_account_touch BEFORE UPDATE ON ledger.gl_account FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- Account settings per legal entity (SAP SKB1). Only accounts listed here can be posted in the entity.
CREATE TABLE ledger.company_account (
  tenant_id             uuid NOT NULL,
  legal_entity_id       uuid NOT NULL,
  account_id            uuid NOT NULL,
  is_posting_blocked    boolean NOT NULL DEFAULT false,
  is_open_item_managed  boolean NOT NULL DEFAULT false,
  created_at            timestamptz NOT NULL DEFAULT now(),
  created_by            uuid NOT NULL,
  updated_at            timestamptz NOT NULL DEFAULT now(),
  updated_by            uuid NOT NULL,
  version               integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, legal_entity_id, account_id),
  FOREIGN KEY (tenant_id, account_id) REFERENCES ledger.gl_account (tenant_id, id)
);
CREATE TRIGGER company_account_touch BEFORE UPDATE ON ledger.company_account FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- Which account automatic postings use (SAP T030).
CREATE TABLE ledger.account_determination (
  tenant_id        uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,
  key              text NOT NULL CHECK (key IN ('ap_reconciliation', 'ar_reconciliation', 'input_tax', 'output_tax', 'bank', 'gr_ir', 'cash_discount', 'fx_gain', 'fx_loss', 'rounding')),
  account_id       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  version          integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, legal_entity_id, key),
  FOREIGN KEY (tenant_id, legal_entity_id, account_id) REFERENCES ledger.company_account (tenant_id, legal_entity_id, account_id)
);
CREATE TRIGGER account_determination_touch BEFORE UPDATE ON ledger.account_determination FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE ledger.tax_code (
  tenant_id        uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,
  code             text NOT NULL CHECK (code ~ '^[A-Z0-9]{1,4}$'),
  description      text NOT NULL,
  direction        text NOT NULL CHECK (direction IN ('input', 'output')),
  rate_pct         numeric(9,6) NOT NULL CHECK (rate_pct >= 0 AND rate_pct < 100),
  account_id       uuid NOT NULL,
  valid_from       date NOT NULL,
  valid_to         date,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, legal_entity_id, code, valid_from),
  FOREIGN KEY (tenant_id, legal_entity_id, account_id) REFERENCES ledger.company_account (tenant_id, legal_entity_id, account_id),
  CHECK (valid_to IS NULL OR valid_to > valid_from),
  EXCLUDE USING gist (tenant_id WITH =, legal_entity_id WITH =, code WITH =, daterange(valid_from, valid_to) WITH &&)
);

CREATE TABLE ledger.exchange_rate (
  tenant_id      uuid NOT NULL,
  id             uuid NOT NULL,
  rate_type      text NOT NULL CHECK (rate_type IN ('average', 'bank_buying', 'bank_selling', 'planning')),   -- SAP M, G, B, P
  from_currency  char(3) NOT NULL REFERENCES ref.currency,
  to_currency    char(3) NOT NULL REFERENCES ref.currency,
  valid_from     date NOT NULL,
  rate           numeric(28,12) NOT NULL CHECK (rate > 0),
  source         text NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now(),
  created_by     uuid NOT NULL,
  PRIMARY KEY (tenant_id, rate_type, from_currency, to_currency, valid_from),
  UNIQUE (tenant_id, id),
  CHECK (from_currency <> to_currency)
);
CREATE TRIGGER exchange_rate_immutable BEFORE UPDATE OR DELETE ON ledger.exchange_rate
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- ---------------------------------------------------------------------------
-- Journal: insert-only, partitioned by fiscal year (ADR-0006)
-- ---------------------------------------------------------------------------

CREATE TABLE ledger.journal_entry (
  tenant_id           uuid NOT NULL,
  fiscal_year         smallint NOT NULL,
  id                  uuid NOT NULL,
  legal_entity_id     uuid NOT NULL,
  ledger_id           uuid NOT NULL,
  fiscal_period       smallint NOT NULL CHECK (fiscal_period BETWEEN 1 AND 16),
  document_type       text NOT NULL CHECK (document_type ~ '^[A-Z0-9]{2}$'),   -- SA general, KR vendor invoice, KZ payment...
  document_number     text NOT NULL,
  document_date       date NOT NULL,
  posting_date        date NOT NULL,
  currency_tc         char(3) NOT NULL REFERENCES ref.currency,
  currency_lc         char(3) NOT NULL REFERENCES ref.currency,
  currency_gc         char(3) NOT NULL REFERENCES ref.currency,
  rate_tc_lc          numeric(28,12) NOT NULL CHECK (rate_tc_lc > 0),
  rate_lc_gc          numeric(28,12) NOT NULL CHECK (rate_lc_gc > 0),
  reference           text,
  header_text         text,
  source_context      text NOT NULL CHECK (source_context IN ('ledger', 'payables', 'payments', 'procurement', 'receivables', 'assets', 'integration')),
  source_document_id  uuid,
  reversal_of_id      uuid,
  reversal_of_year    smallint,
  posted_at           timestamptz NOT NULL DEFAULT now(),
  posted_by           uuid NOT NULL,
  PRIMARY KEY (tenant_id, fiscal_year, id),
  UNIQUE (tenant_id, fiscal_year, legal_entity_id, document_type, document_number),
  FOREIGN KEY (tenant_id, ledger_id) REFERENCES ledger.ledger (tenant_id, id),
  FOREIGN KEY (tenant_id, reversal_of_year, reversal_of_id) REFERENCES ledger.journal_entry (tenant_id, fiscal_year, id),
  CHECK ((reversal_of_id IS NULL) = (reversal_of_year IS NULL))
) PARTITION BY RANGE (fiscal_year);
CREATE INDEX journal_entry_source ON ledger.journal_entry (tenant_id, source_context, source_document_id);
CREATE INDEX journal_entry_posting ON ledger.journal_entry (tenant_id, legal_entity_id, posting_date);

CREATE TABLE ledger.journal_line (
  tenant_id         uuid NOT NULL,
  fiscal_year       smallint NOT NULL,
  journal_entry_id  uuid NOT NULL,
  line_no           integer NOT NULL CHECK (line_no > 0),
  legal_entity_id   uuid NOT NULL,                  -- denormalized from the entry for indexes
  ledger_id         uuid NOT NULL,
  fiscal_period     smallint NOT NULL,
  posting_date      date NOT NULL,
  account_id        uuid NOT NULL,
  amount_tc         bigint NOT NULL,                -- signed: debit positive, credit negative
  amount_lc         bigint NOT NULL,
  amount_gc         bigint NOT NULL,
  partner_id        uuid,                           -- partner context: supplier or customer on reconciliation lines
  tax_code          text,
  quantity          numeric(20,6),
  uom_code          text REFERENCES ref.unit_of_measure,
  line_text         text,
  -- Controlling dimensions (Phase 2) are added here as nullable columns,
  -- keeping ledger and controlling one table as in SAP's universal journal.
  PRIMARY KEY (tenant_id, fiscal_year, journal_entry_id, line_no),
  FOREIGN KEY (tenant_id, fiscal_year, journal_entry_id) REFERENCES ledger.journal_entry (tenant_id, fiscal_year, id),
  FOREIGN KEY (tenant_id, legal_entity_id, account_id) REFERENCES ledger.company_account (tenant_id, legal_entity_id, account_id),
  CHECK ((quantity IS NULL) = (uom_code IS NULL))
) PARTITION BY RANGE (fiscal_year);
CREATE INDEX journal_line_account ON ledger.journal_line (tenant_id, legal_entity_id, account_id, fiscal_period);
CREATE INDEX journal_line_partner ON ledger.journal_line (tenant_id, legal_entity_id, partner_id) WHERE partner_id IS NOT NULL;

-- Yearly partitions; a maintenance job creates future years ahead of time.
DO $$
BEGIN
  FOR y IN 2015..2035 LOOP
    EXECUTE format('CREATE TABLE ledger.journal_entry_y%s PARTITION OF ledger.journal_entry FOR VALUES FROM (%s) TO (%s)', y, y, y + 1);
    EXECUTE format('CREATE TABLE ledger.journal_line_y%s PARTITION OF ledger.journal_line FOR VALUES FROM (%s) TO (%s)', y, y, y + 1);
  END LOOP;
END $$;

-- An entry can be reversed at most once, across fiscal years.
CREATE TABLE ledger.reversal (
  tenant_id              uuid NOT NULL,
  original_year          smallint NOT NULL,
  original_entry_id      uuid NOT NULL,
  reversal_year          smallint NOT NULL,
  reversal_entry_id      uuid NOT NULL,
  PRIMARY KEY (tenant_id, original_entry_id),
  FOREIGN KEY (tenant_id, original_year, original_entry_id) REFERENCES ledger.journal_entry (tenant_id, fiscal_year, id),
  FOREIGN KEY (tenant_id, reversal_year, reversal_entry_id) REFERENCES ledger.journal_entry (tenant_id, fiscal_year, id)
);

-- Postings go only into open periods, with dates inside the period.
CREATE FUNCTION ledger.check_entry_period() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE st text; d_start date; d_end date;
BEGIN
  SELECT ps.status, fp.start_date, fp.end_date INTO st, d_start, d_end
  FROM ledger.period_status ps
  JOIN ledger.fiscal_period fp ON fp.tenant_id = ps.tenant_id AND fp.calendar_id = ps.calendar_id
       AND fp.fiscal_year = ps.fiscal_year AND fp.period = ps.period
  WHERE ps.tenant_id = NEW.tenant_id AND ps.legal_entity_id = NEW.legal_entity_id AND ps.ledger_id = NEW.ledger_id
    AND ps.fiscal_year = NEW.fiscal_year AND ps.period = NEW.fiscal_period;
  IF st IS NULL THEN
    RAISE EXCEPTION 'period %/% is not defined for this legal entity and ledger', NEW.fiscal_year, NEW.fiscal_period USING ERRCODE = 'check_violation';
  END IF;
  IF st <> 'open' THEN
    RAISE EXCEPTION 'period %/% is %', NEW.fiscal_year, NEW.fiscal_period, st USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.posting_date NOT BETWEEN d_start AND d_end THEN
    RAISE EXCEPTION 'posting date % is outside period %/% (% to %)', NEW.posting_date, NEW.fiscal_year, NEW.fiscal_period, d_start, d_end
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER journal_entry_period BEFORE INSERT ON ledger.journal_entry FOR EACH ROW EXECUTE FUNCTION ledger.check_entry_period();

-- Lines agree with their entry; reconciliation accounts need a partner; blocked accounts refuse postings.
CREATE FUNCTION ledger.check_line() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE e ledger.journal_entry%ROWTYPE; kind text; blocked boolean;
BEGIN
  SELECT * INTO e FROM ledger.journal_entry WHERE tenant_id = NEW.tenant_id AND fiscal_year = NEW.fiscal_year AND id = NEW.journal_entry_id;
  IF NEW.legal_entity_id <> e.legal_entity_id OR NEW.ledger_id <> e.ledger_id OR NEW.fiscal_period <> e.fiscal_period OR NEW.posting_date <> e.posting_date THEN
    RAISE EXCEPTION 'line % does not match its journal entry header', NEW.line_no USING ERRCODE = 'check_violation';
  END IF;
  SELECT a.reconciliation_kind, ca.is_posting_blocked OR a.status <> 'active' INTO kind, blocked
  FROM ledger.gl_account a JOIN ledger.company_account ca ON ca.tenant_id = a.tenant_id AND ca.account_id = a.id
  WHERE a.tenant_id = NEW.tenant_id AND a.id = NEW.account_id AND ca.legal_entity_id = NEW.legal_entity_id;
  IF blocked THEN
    RAISE EXCEPTION 'account is blocked for posting' USING ERRCODE = 'check_violation';
  END IF;
  IF kind IN ('supplier', 'customer') AND NEW.partner_id IS NULL THEN
    RAISE EXCEPTION 'line % posts to a % reconciliation account without a partner', NEW.line_no, kind USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER journal_line_check BEFORE INSERT ON ledger.journal_line FOR EACH ROW EXECUTE FUNCTION ledger.check_line();

-- At commit, every entry has at least two lines and balances in all three currencies.
CREATE FUNCTION ledger.check_entry_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE n integer; tc bigint; lc bigint; gc bigint;
BEGIN
  SELECT count(*), coalesce(sum(amount_tc), 0), coalesce(sum(amount_lc), 0), coalesce(sum(amount_gc), 0)
  INTO n, tc, lc, gc
  FROM ledger.journal_line WHERE tenant_id = NEW.tenant_id AND fiscal_year = NEW.fiscal_year AND journal_entry_id = NEW.id;
  IF n < 2 THEN
    RAISE EXCEPTION 'journal entry % has % line(s); at least two are required', NEW.document_number, n USING ERRCODE = 'check_violation';
  END IF;
  IF tc <> 0 OR lc <> 0 OR gc <> 0 THEN
    RAISE EXCEPTION 'journal entry % is unbalanced (tc %, lc %, gc %)', NEW.document_number, tc, lc, gc USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER journal_entry_balanced AFTER INSERT ON ledger.journal_entry
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger.check_entry_balanced();

CREATE TRIGGER journal_entry_immutable BEFORE UPDATE OR DELETE ON ledger.journal_entry FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER journal_line_immutable BEFORE UPDATE OR DELETE ON ledger.journal_line FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER reversal_immutable BEFORE UPDATE OR DELETE ON ledger.reversal FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- ---------------------------------------------------------------------------
-- Clearing (open items settled), insert-only
-- ---------------------------------------------------------------------------

CREATE TABLE ledger.clearing (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  legal_entity_id      uuid NOT NULL,
  clearing_date        date NOT NULL,
  clearing_entry_year  smallint NOT NULL,
  clearing_entry_id    uuid NOT NULL,
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, clearing_entry_year, clearing_entry_id) REFERENCES ledger.journal_entry (tenant_id, fiscal_year, id)
);

CREATE TABLE ledger.clearing_item (
  tenant_id         uuid NOT NULL,
  fiscal_year       smallint NOT NULL,
  journal_entry_id  uuid NOT NULL,
  line_no           integer NOT NULL,
  clearing_id       uuid NOT NULL,
  PRIMARY KEY (tenant_id, fiscal_year, journal_entry_id, line_no),     -- a line is cleared at most once
  FOREIGN KEY (tenant_id, fiscal_year, journal_entry_id, line_no) REFERENCES ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no),
  FOREIGN KEY (tenant_id, clearing_id) REFERENCES ledger.clearing (tenant_id, id)
);
CREATE INDEX clearing_item_by_clearing ON ledger.clearing_item (tenant_id, clearing_id);
CREATE TRIGGER clearing_immutable BEFORE UPDATE OR DELETE ON ledger.clearing FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER clearing_item_immutable BEFORE UPDATE OR DELETE ON ledger.clearing_item FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- Open items: lines on supplier/customer reconciliation accounts not yet cleared.
CREATE VIEW ledger.open_item WITH (security_invoker = true) AS
SELECT l.tenant_id, l.fiscal_year, l.journal_entry_id, l.line_no, l.legal_entity_id, l.account_id, l.partner_id,
       l.posting_date, l.amount_tc, l.amount_lc, e.currency_tc, e.document_number, e.reference
FROM ledger.journal_line l
JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
JOIN ledger.gl_account a ON a.tenant_id = l.tenant_id AND a.id = l.account_id AND a.reconciliation_kind IN ('supplier', 'customer')
WHERE NOT EXISTS (
  SELECT 1 FROM ledger.clearing_item c
  WHERE c.tenant_id = l.tenant_id AND c.fiscal_year = l.fiscal_year AND c.journal_entry_id = l.journal_entry_id AND c.line_no = l.line_no);

-- Period balances, maintained by an idempotent projector from JournalEntryPosted events.
CREATE TABLE ledger.period_balance (
  tenant_id        uuid NOT NULL,
  ledger_id        uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,
  account_id       uuid NOT NULL,
  fiscal_year      smallint NOT NULL,
  fiscal_period    smallint NOT NULL,
  currency_tc      char(3) NOT NULL REFERENCES ref.currency,
  debit_tc         bigint NOT NULL DEFAULT 0 CHECK (debit_tc >= 0),
  credit_tc        bigint NOT NULL DEFAULT 0 CHECK (credit_tc >= 0),
  debit_lc         bigint NOT NULL DEFAULT 0 CHECK (debit_lc >= 0),
  credit_lc        bigint NOT NULL DEFAULT 0 CHECK (credit_lc >= 0),
  debit_gc         bigint NOT NULL DEFAULT 0 CHECK (debit_gc >= 0),
  credit_gc        bigint NOT NULL DEFAULT 0 CHECK (credit_gc >= 0),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, ledger_id, legal_entity_id, account_id, fiscal_year, fiscal_period, currency_tc)
);

SELECT platform.create_event_tables('ledger');
