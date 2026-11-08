-- The trading partner on journal lines (T68b; ADR-0059 §4; 03-data-model
-- §3.7): the tenant's company on the other side of an intercompany
-- transaction, as SAP's ACDOCA carries RASSC. A legal entity id without a
-- foreign key (tenancy is another context, 02 §2.1): the ledger checks
-- that it is a legal entity of the tenant before it posts, and the line's
-- check below that it is not the line's own company. Expand only
-- (06 §6.4): nullable columns without a default rewrite no partition, and
-- the check is added NOT VALID, reading none of the rows under the lock;
-- 000220 validates it partition by partition and 000221 indexes the lines
-- by trading partner. Lines posted before keep NULL; nothing is backfilled.
ALTER TABLE ledger.journal_line ADD COLUMN trading_partner_id uuid;
-- Added to the parent, it is each partition's too, and a partition created
-- later (db.EnsurePartitions) gets it validated, being empty.
ALTER TABLE ledger.journal_line ADD CONSTRAINT journal_line_trading_partner
  CHECK (trading_partner_id <> legal_entity_id) NOT VALID;

-- Entered on a parked entry's and a template's lines (ADR-0059 §4), kept
-- as entered and checked again when the entry is posted, as their centers
-- are (000159, 000184).
ALTER TABLE ledger.parked_entry_line ADD COLUMN trading_partner_id uuid;
ALTER TABLE ledger.entry_template_line ADD COLUMN trading_partner_id uuid;

-- Balances by trading partner (ADR-0059 §4): per ledger, company, account,
-- trading partner, year, period and transaction currency, the totals of
-- the lines that carry one, beside period_balance as assignment_balance is
-- (000163). Consolidation reads them, with the lines not projected yet,
-- per account and trading partner (ADR-0059 §4a, T68d).
CREATE TABLE ledger.trading_partner_balance (
  tenant_id          uuid NOT NULL,
  ledger_id          uuid NOT NULL,
  legal_entity_id    uuid NOT NULL,
  account_id         uuid NOT NULL,
  trading_partner_id uuid NOT NULL,
  fiscal_year        smallint NOT NULL,
  fiscal_period      smallint NOT NULL,
  currency_tc        char(3) NOT NULL REFERENCES ref.currency,
  debit_tc           bigint NOT NULL DEFAULT 0 CHECK (debit_tc >= 0),
  credit_tc          bigint NOT NULL DEFAULT 0 CHECK (credit_tc >= 0),
  debit_lc           bigint NOT NULL DEFAULT 0 CHECK (debit_lc >= 0),
  credit_lc          bigint NOT NULL DEFAULT 0 CHECK (credit_lc >= 0),
  debit_gc           bigint NOT NULL DEFAULT 0 CHECK (debit_gc >= 0),
  credit_gc          bigint NOT NULL DEFAULT 0 CHECK (credit_gc >= 0),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, ledger_id, legal_entity_id, account_id, trading_partner_id, fiscal_year, fiscal_period, currency_tc),
  CONSTRAINT trading_partner_balance_partner CHECK (trading_partner_id <> legal_entity_id)
);
-- A company's balances of a year through a period (T68d's read), whatever
-- the account and partner.
CREATE INDEX trading_partner_balance_company ON ledger.trading_partner_balance (tenant_id, legal_entity_id, ledger_id, fiscal_year, fiscal_period);
SELECT platform.secure_table('ledger.trading_partner_balance');
-- Only the projection writes balances.
REVOKE INSERT ON ledger.trading_partner_balance FROM erp_app;
GRANT SELECT, INSERT, UPDATE ON ledger.trading_partner_balance TO erp_projector;

-- Marking a batch of entries projected adds their lines with a trading
-- partner here in the same statement, as 000163's trigger adds the
-- assigned lines to assignment_balance: counted exactly once, in the
-- projector's transaction, by every release's projector. Only an entry's
-- first projection counts; a row still unprojected is left to the reads,
-- which add it. Balances are taken in key order, so replicas projecting
-- side by side wait for each other, never deadlock. No line carries a
-- trading partner before this migration, so nothing is backfilled.
CREATE FUNCTION ledger.project_trading_partner_balance() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO ledger.trading_partner_balance AS tb (tenant_id, ledger_id, legal_entity_id, account_id, trading_partner_id, fiscal_year,
    fiscal_period, currency_tc, debit_tc, credit_tc, debit_lc, credit_lc, debit_gc, credit_gc)
  SELECT l.tenant_id, l.ledger_id, l.legal_entity_id, l.account_id, l.trading_partner_id, l.fiscal_year, l.fiscal_period, e.currency_tc,
    sum(greatest(l.amount_tc, 0)), sum(greatest(-l.amount_tc, 0)), sum(greatest(l.amount_lc, 0)), sum(greatest(-l.amount_lc, 0)),
    sum(greatest(l.amount_gc, 0)), sum(greatest(-l.amount_gc, 0))
  FROM projected n
  JOIN unprojected o ON o.tenant_id = n.tenant_id AND o.event_id = n.event_id
  JOIN ledger.journal_entry e ON e.tenant_id = n.tenant_id AND e.fiscal_year = n.fiscal_year AND e.id = n.journal_entry_id
  JOIN ledger.journal_line l ON l.tenant_id = n.tenant_id AND l.fiscal_year = n.fiscal_year AND l.journal_entry_id = n.journal_entry_id
  WHERE o.projected_at IS NULL AND n.projected_at IS NOT NULL AND l.trading_partner_id IS NOT NULL
  GROUP BY 1, 2, 3, 4, 5, 6, 7, 8 ORDER BY 1, 2, 3, 4, 5, 6, 7, 8
  ON CONFLICT (tenant_id, ledger_id, legal_entity_id, account_id, trading_partner_id, fiscal_year, fiscal_period, currency_tc) DO UPDATE SET
    debit_tc = tb.debit_tc + EXCLUDED.debit_tc, credit_tc = tb.credit_tc + EXCLUDED.credit_tc,
    debit_lc = tb.debit_lc + EXCLUDED.debit_lc, credit_lc = tb.credit_lc + EXCLUDED.credit_lc,
    debit_gc = tb.debit_gc + EXCLUDED.debit_gc, credit_gc = tb.credit_gc + EXCLUDED.credit_gc, updated_at = now();
  RETURN NULL;
END $$;
CREATE TRIGGER balance_projection_trading_partner AFTER UPDATE ON ledger.balance_projection
  REFERENCING OLD TABLE AS unprojected NEW TABLE AS projected FOR EACH STATEMENT EXECUTE FUNCTION ledger.project_trading_partner_balance();
