-- Foreign-currency revaluation (T48a, ADR-0042, 03-data-model §3.7): the
-- settings a revaluation run (SAP FAGL_FCV) reads, kept per legal entity by
-- the ledger's setup from the configuration, and the runs and the items
-- they valued. Expand only: new tables and a column with a default.

-- G/L accounts other than reconciliation accounts that are revalued (SAP
-- SKB1 "open item / balance in foreign currency" valuation): assets and
-- liabilities that reconcile no partner, valued by balance per currency.
ALTER TABLE ledger.company_account ADD COLUMN revalue_foreign_currency boolean NOT NULL DEFAULT false;

-- The rate type a legal entity values at (one per entity, ADR-0042).
CREATE TABLE ledger.fx_valuation (
  tenant_id        uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,                  -- tenancy context
  rate_type        text NOT NULL DEFAULT 'average' CHECK (rate_type IN ('average', 'bank_buying', 'bank_selling', 'planning')),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  version          integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, legal_entity_id)
);
CREATE TRIGGER fx_valuation_touch BEFORE UPDATE ON ledger.fx_valuation FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.fx_valuation'::regclass);

-- Where the valuation of one revalued account posts (SAP T030S, KDB/KDF):
-- the unrealised gain and loss, kept apart from the realised fx_gain and
-- fx_loss of the account determination though they may name the same
-- accounts, and, for a reconciliation account, the balance-sheet
-- adjustment account the difference goes to, since a reconciliation account
-- is posted only through its partners' items. A flagged G/L account takes
-- the difference itself and has no adjustment account.
CREATE TABLE ledger.fx_valuation_account (
  tenant_id              uuid NOT NULL,
  legal_entity_id        uuid NOT NULL,
  account_id             uuid NOT NULL,           -- the revalued account
  adjustment_account_id  uuid,
  gain_account_id        uuid NOT NULL,
  loss_account_id        uuid NOT NULL,
  is_active              boolean NOT NULL DEFAULT true,  -- false once the configuration no longer has it
  created_at             timestamptz NOT NULL DEFAULT now(),
  created_by             uuid NOT NULL,
  updated_at             timestamptz NOT NULL DEFAULT now(),
  updated_by             uuid NOT NULL,
  version                integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, legal_entity_id, account_id),
  FOREIGN KEY (tenant_id, legal_entity_id, account_id) REFERENCES ledger.company_account (tenant_id, legal_entity_id, account_id),
  FOREIGN KEY (tenant_id, legal_entity_id, adjustment_account_id) REFERENCES ledger.company_account (tenant_id, legal_entity_id, account_id),
  FOREIGN KEY (tenant_id, legal_entity_id, gain_account_id) REFERENCES ledger.company_account (tenant_id, legal_entity_id, account_id),
  FOREIGN KEY (tenant_id, legal_entity_id, loss_account_id) REFERENCES ledger.company_account (tenant_id, legal_entity_id, account_id)
);
CREATE TRIGGER fx_valuation_account_touch BEFORE UPDATE ON ledger.fx_valuation_account FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- An active valuation is of a supplier or customer reconciliation account,
-- with a balance-sheet adjustment account that reconciles nothing, or of a
-- flagged account, without one; its gain is a revenue and its loss an
-- expense.
CREATE FUNCTION ledger.check_fx_valuation_account() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  recon boolean;
  flagged boolean;
BEGIN
  IF NOT NEW.is_active THEN
    RETURN NEW;
  END IF;
  SELECT g.reconciliation_kind IN ('supplier', 'customer'), c.revalue_foreign_currency INTO recon, flagged
    FROM ledger.company_account c JOIN ledger.gl_account g ON g.tenant_id = c.tenant_id AND g.id = c.account_id
    WHERE c.tenant_id = NEW.tenant_id AND c.legal_entity_id = NEW.legal_entity_id AND c.account_id = NEW.account_id;
  IF NOT FOUND THEN
    RETURN NEW;  -- the foreign key refuses it
  END IF;
  IF NOT (recon OR flagged) THEN
    RAISE EXCEPTION 'account % is revalued only as a supplier or customer reconciliation account or flagged revalue_foreign_currency', NEW.account_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'fx_valuation_account_revalued';
  END IF;
  IF recon <> (NEW.adjustment_account_id IS NOT NULL) OR (recon AND NOT EXISTS (SELECT FROM ledger.gl_account g
      WHERE g.tenant_id = NEW.tenant_id AND g.id = NEW.adjustment_account_id AND g.is_balance_sheet AND g.reconciliation_kind = 'none')) THEN
    RAISE EXCEPTION 'account % has a balance-sheet adjustment account that reconciles nothing exactly when it is a reconciliation account', NEW.account_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'fx_valuation_account_adjustment';
  END IF;
  IF NOT EXISTS (SELECT FROM ledger.gl_account g WHERE g.tenant_id = NEW.tenant_id AND g.id = NEW.gain_account_id AND g.account_type = 'revenue') THEN
    RAISE EXCEPTION 'the unrealised gain of account % is a revenue account', NEW.account_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'fx_valuation_account_gain';
  END IF;
  IF NOT EXISTS (SELECT FROM ledger.gl_account g WHERE g.tenant_id = NEW.tenant_id AND g.id = NEW.loss_account_id AND g.account_type = 'expense') THEN
    RAISE EXCEPTION 'the unrealised loss of account % is an expense account', NEW.account_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'fx_valuation_account_loss';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_fx_valuation_account() OWNER TO erp_owner;
CREATE TRIGGER fx_valuation_account_check BEFORE INSERT OR UPDATE ON ledger.fx_valuation_account
  FOR EACH ROW EXECUTE FUNCTION ledger.check_fx_valuation_account();
SELECT platform.secure_table('ledger.fx_valuation_account'::regclass);

-- A flagged account is an asset or liability that reconciles no partner
-- (reconciliation accounts are revalued through their items; equity is
-- non-monetary, IAS 21.16, and kept at historical rates); its flag
-- stays while an active valuation names it.
CREATE FUNCTION ledger.check_revalue_foreign_currency() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.revalue_foreign_currency AND NOT EXISTS (SELECT FROM ledger.gl_account g WHERE g.tenant_id = NEW.tenant_id AND g.id = NEW.account_id
      AND g.account_type IN ('asset', 'liability') AND g.reconciliation_kind = 'none') THEN
    RAISE EXCEPTION 'account % is revalued in foreign currency only as an asset or liability that reconciles no partner', NEW.account_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'company_account_revalue_foreign_currency';
  END IF;
  IF TG_OP = 'UPDATE' AND OLD.revalue_foreign_currency AND NOT NEW.revalue_foreign_currency AND EXISTS (SELECT FROM ledger.fx_valuation_account v
      WHERE v.tenant_id = NEW.tenant_id AND v.legal_entity_id = NEW.legal_entity_id AND v.account_id = NEW.account_id AND v.is_active) THEN
    RAISE EXCEPTION 'account % keeps revalue_foreign_currency while its foreign-currency valuation is active', NEW.account_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'company_account_revalue_foreign_currency_valued';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_revalue_foreign_currency() OWNER TO erp_owner;
CREATE TRIGGER company_account_revalue_foreign_currency BEFORE INSERT OR UPDATE OF revalue_foreign_currency ON ledger.company_account
  FOR EACH ROW EXECUTE FUNCTION ledger.check_revalue_foreign_currency();

-- A revaluation run of one legal entity and ledger at a key date: the
-- valuation entry dated the key date and its reversal dated the next day,
-- the first of the next period, both of the run's legal entity and ledger.
-- A run that found no difference posts neither. At most one run per key
-- date is posted; a cancelled run frees it for a rerun.
CREATE TABLE ledger.revaluation_run (
  tenant_id             uuid NOT NULL,
  id                    uuid NOT NULL,
  legal_entity_id       uuid NOT NULL,           -- tenancy context
  ledger_id             uuid NOT NULL,
  key_date              date NOT NULL,
  rate_type             text NOT NULL CHECK (rate_type IN ('average', 'bank_buying', 'bank_selling', 'planning')),
  status                text NOT NULL DEFAULT 'posted' CHECK (status IN ('posted', 'cancelled')),
  valuation_entry_year  smallint,
  valuation_entry_id    uuid,
  reversal_entry_year   smallint,
  reversal_entry_id     uuid,
  created_at            timestamptz NOT NULL DEFAULT now(),
  created_by            uuid NOT NULL,
  updated_at            timestamptz NOT NULL DEFAULT now(),
  updated_by            uuid NOT NULL,
  version               integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, ledger_id) REFERENCES ledger.ledger (tenant_id, id),
  FOREIGN KEY (tenant_id, valuation_entry_year, valuation_entry_id) REFERENCES ledger.journal_entry (tenant_id, fiscal_year, id),
  FOREIGN KEY (tenant_id, reversal_entry_year, reversal_entry_id) REFERENCES ledger.journal_entry (tenant_id, fiscal_year, id),
  CONSTRAINT revaluation_run_entries CHECK (num_nulls(valuation_entry_year, valuation_entry_id, reversal_entry_year, reversal_entry_id) IN (0, 4))
);
CREATE UNIQUE INDEX revaluation_run_posted ON ledger.revaluation_run (tenant_id, legal_entity_id, ledger_id, key_date) WHERE status = 'posted';
-- An entry is one run's: a rerun after a cancellation posts its own.
CREATE UNIQUE INDEX revaluation_run_valuation_entry ON ledger.revaluation_run (tenant_id, valuation_entry_year, valuation_entry_id) WHERE valuation_entry_id IS NOT NULL;
CREATE UNIQUE INDEX revaluation_run_reversal_entry ON ledger.revaluation_run (tenant_id, reversal_entry_year, reversal_entry_id) WHERE reversal_entry_id IS NOT NULL;
CREATE TRIGGER revaluation_run_touch BEFORE UPDATE ON ledger.revaluation_run FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- A run is posted first, may be cancelled once, and never changes what it
-- valued or posted. Callers read it FOR UPDATE before they cancel it
-- (ADR-0008).
CREATE FUNCTION ledger.check_revaluation_run() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'posted' THEN
      RAISE EXCEPTION 'a revaluation run is posted first, not %', NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'revaluation_run_lifecycle';
    END IF;
    IF num_nulls(NEW.valuation_entry_year, NEW.valuation_entry_id, NEW.reversal_entry_year, NEW.reversal_entry_id) = 0 AND (
        NOT EXISTS (SELECT FROM ledger.journal_entry e WHERE e.tenant_id = NEW.tenant_id AND e.fiscal_year = NEW.valuation_entry_year
          AND e.id = NEW.valuation_entry_id AND e.legal_entity_id = NEW.legal_entity_id AND e.ledger_id = NEW.ledger_id AND e.posting_date = NEW.key_date)
        OR NOT EXISTS (SELECT FROM ledger.journal_entry e WHERE e.tenant_id = NEW.tenant_id AND e.fiscal_year = NEW.reversal_entry_year
          AND e.id = NEW.reversal_entry_id AND e.legal_entity_id = NEW.legal_entity_id AND e.ledger_id = NEW.ledger_id AND e.posting_date = NEW.key_date + 1)) THEN
      RAISE EXCEPTION 'revaluation run %: the valuation entry is dated the key date and its reversal the next day, both of its legal entity and ledger', NEW.id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'revaluation_run_entry_dates';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.status IS DISTINCT FROM OLD.status AND (OLD.status, NEW.status) NOT IN (('posted', 'cancelled')) THEN
    RAISE EXCEPTION 'revaluation run % cannot go from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'revaluation_run_lifecycle';
  END IF;
  IF (NEW.legal_entity_id, NEW.ledger_id, NEW.key_date, NEW.rate_type, NEW.valuation_entry_year, NEW.valuation_entry_id, NEW.reversal_entry_year,
      NEW.reversal_entry_id, NEW.created_at, NEW.created_by)
     IS DISTINCT FROM
     (OLD.legal_entity_id, OLD.ledger_id, OLD.key_date, OLD.rate_type, OLD.valuation_entry_year, OLD.valuation_entry_id, OLD.reversal_entry_year,
      OLD.reversal_entry_id, OLD.created_at, OLD.created_by) THEN
    RAISE EXCEPTION 'revaluation run % is fixed once posted', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'revaluation_run_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_revaluation_run() OWNER TO erp_owner;
CREATE TRIGGER revaluation_run_check BEFORE INSERT OR UPDATE ON ledger.revaluation_run
  FOR EACH ROW EXECUTE FUNCTION ledger.check_revaluation_run();
SELECT platform.secure_table('ledger.revaluation_run'::regclass);

-- What a run valued: an open item (a journal line on a reconciliation
-- account) or the balance of a flagged account in one currency, its
-- transaction-currency amount and booked local and group amounts, and the
-- key date's rates into local and into group currency with the amounts at
-- them: group currency is valued on its own (SAP parallel-currency
-- valuation), not translated from the local difference. A balance whose
-- transaction-currency amount is zero is valued while its local or group
-- amount is not: that residual is what the run corrects. Insert-only.
CREATE TABLE ledger.revaluation_item (
  tenant_id         uuid NOT NULL,
  run_id            uuid NOT NULL,
  item_no           integer NOT NULL CHECK (item_no > 0),
  account_id        uuid NOT NULL,
  partner_id        uuid,                         -- partner context: the open item's supplier or customer
  fiscal_year       smallint,                     -- the open item; none for a flagged account's balance
  journal_entry_id  uuid,
  line_no           integer,
  currency_tc       char(3) NOT NULL REFERENCES ref.currency,
  amount_tc         bigint NOT NULL,
  amount_lc         bigint NOT NULL,              -- booked, before this run
  amount_gc         bigint NOT NULL,              -- booked, before this run
  rate              numeric(28,12) CHECK (rate > 0), -- none for a balance zero in its currency, valued without rates
  rate_valid_from   date,
  valued_lc         bigint NOT NULL,              -- amount_tc at rate
  rate_gc           numeric(28,12) CHECK (rate_gc > 0),
  rate_gc_valid_from date,                        -- none when the item's currency is the group's (rate 1)
  valued_gc         bigint NOT NULL,              -- amount_tc at rate_gc
  PRIMARY KEY (tenant_id, run_id, item_no),
  FOREIGN KEY (tenant_id, run_id) REFERENCES ledger.revaluation_run (tenant_id, id),
  CONSTRAINT revaluation_item_open_item FOREIGN KEY (tenant_id, fiscal_year, journal_entry_id, line_no) REFERENCES ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no),
  CONSTRAINT revaluation_item_kind CHECK (num_nulls(fiscal_year, journal_entry_id, line_no) IN (0, 3)
    AND (journal_entry_id IS NOT NULL OR partner_id IS NULL)),
  -- An open item has an amount in its currency; a balance has one in some
  -- currency.
  -- Only the group's own currency is valued at 1 without a recorded rate.
  CONSTRAINT revaluation_item_rate_gc CHECK (rate_gc_valid_from IS NOT NULL OR rate_gc = 1),
  -- A balance zero in its currency is worth zero at any rate, so it is
  -- valued without one (a rate no longer kept does not stop the run); every
  -- other item has both rates.
  CONSTRAINT revaluation_item_rates CHECK ((rate IS NULL) = (amount_tc = 0) AND (rate IS NULL) = (rate_valid_from IS NULL)
    AND (rate IS NULL) = (rate_gc IS NULL) AND (rate_gc IS NOT NULL OR rate_gc_valid_from IS NULL)),
  CONSTRAINT revaluation_item_amount CHECK (amount_tc <> 0 OR (journal_entry_id IS NULL AND (amount_lc <> 0 OR amount_gc <> 0)))
);
CREATE UNIQUE INDEX revaluation_item_line ON ledger.revaluation_item (tenant_id, run_id, fiscal_year, journal_entry_id, line_no)
  WHERE journal_entry_id IS NOT NULL;
CREATE UNIQUE INDEX revaluation_item_balance ON ledger.revaluation_item (tenant_id, run_id, account_id, currency_tc)
  WHERE journal_entry_id IS NULL;
-- The last run that valued an open item is what the next run and the
-- clearing of the item read (T48b).
CREATE INDEX revaluation_item_by_line ON ledger.revaluation_item (tenant_id, fiscal_year, journal_entry_id, line_no) WHERE journal_entry_id IS NOT NULL;
CREATE TRIGGER revaluation_item_immutable BEFORE UPDATE OR DELETE ON ledger.revaluation_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('ledger.revaluation_item'::regclass);
