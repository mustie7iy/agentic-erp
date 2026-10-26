-- Balances by account assignment (T56f; ADR-0050 §7; 03-data-model §3.7):
-- per ledger, company, account, cost center, profit center, segment, year,
-- period and transaction currency, the totals of the lines that carry any
-- center. period_balance is unchanged, so the trial balance and the ledger
-- reconciliation do not group the finer key. A line may carry no cost
-- center, so the key's centers are nullable and the primary key is a hash
-- of the whole key, as gr_ir_position's id.
CREATE TABLE ledger.assignment_balance (
  tenant_id        uuid NOT NULL,
  ledger_id        uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,
  account_id       uuid NOT NULL,
  cost_center_id   uuid,
  profit_center_id uuid,
  segment_id       uuid,
  fiscal_year      smallint NOT NULL,
  fiscal_period    smallint NOT NULL,
  currency_tc      char(3) NOT NULL REFERENCES ref.currency,
  id               uuid GENERATED ALWAYS AS (md5(ledger_id::text || '/' || legal_entity_id::text || '/' || account_id::text || '/'
                     || coalesce(cost_center_id::text, '-') || '/' || coalesce(profit_center_id::text, '-') || '/' || coalesce(segment_id::text, '-')
                     || '/' || fiscal_year || '/' || fiscal_period || '/' || currency_tc)::uuid) STORED,
  debit_tc         bigint NOT NULL DEFAULT 0 CHECK (debit_tc >= 0),
  credit_tc        bigint NOT NULL DEFAULT 0 CHECK (credit_tc >= 0),
  debit_lc         bigint NOT NULL DEFAULT 0 CHECK (debit_lc >= 0),
  credit_lc        bigint NOT NULL DEFAULT 0 CHECK (credit_lc >= 0),
  debit_gc         bigint NOT NULL DEFAULT 0 CHECK (debit_gc >= 0),
  credit_gc        bigint NOT NULL DEFAULT 0 CHECK (credit_gc >= 0),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT assignment_balance_assigned CHECK (cost_center_id IS NOT NULL OR profit_center_id IS NOT NULL OR segment_id IS NOT NULL)
);
-- The two reads (GET /v1/cost-center-balances, /v1/profit-center-balances)
-- take one company, ledger and year, a period range and optionally centers.
-- The table is new, so its indexes are built here rather than concurrently.
CREATE INDEX assignment_balance_cost_center ON ledger.assignment_balance (tenant_id, legal_entity_id, ledger_id, fiscal_year, fiscal_period, cost_center_id)
  WHERE cost_center_id IS NOT NULL;
CREATE INDEX assignment_balance_profit_center ON ledger.assignment_balance (tenant_id, legal_entity_id, ledger_id, fiscal_year, fiscal_period, profit_center_id)
  WHERE profit_center_id IS NOT NULL;
SELECT platform.secure_table('ledger.assignment_balance');
-- Only the projection writes balances.
REVOKE INSERT ON ledger.assignment_balance FROM erp_app;
GRANT SELECT, INSERT, UPDATE ON ledger.assignment_balance TO erp_projector;

-- The projector (ADR-0006) marks a batch of queued entries projected and
-- adds their lines to period_balance in one statement. This trigger adds
-- the same entries' assigned lines to assignment_balance in that statement,
-- so both are counted exactly once, in one transaction, by every release's
-- projector: the previous release's, which knows nothing of the table,
-- keeps it during a rolling upgrade too (06 §6.4). Only an entry's first
-- projection counts; nothing sets projected_at twice or clears it, and a
-- row still unprojected is left to the balance reads, which add it.
-- Balances are taken in key order, as the projector takes period_balance's,
-- so replicas projecting side by side wait for each other, never deadlock.
CREATE FUNCTION ledger.project_assignment_balance() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO ledger.assignment_balance AS ab (tenant_id, ledger_id, legal_entity_id, account_id, cost_center_id, profit_center_id, segment_id,
    fiscal_year, fiscal_period, currency_tc, debit_tc, credit_tc, debit_lc, credit_lc, debit_gc, credit_gc)
  SELECT l.tenant_id, l.ledger_id, l.legal_entity_id, l.account_id, l.cost_center_id, l.profit_center_id, l.segment_id, l.fiscal_year, l.fiscal_period,
    e.currency_tc, sum(greatest(l.amount_tc, 0)), sum(greatest(-l.amount_tc, 0)), sum(greatest(l.amount_lc, 0)), sum(greatest(-l.amount_lc, 0)),
    sum(greatest(l.amount_gc, 0)), sum(greatest(-l.amount_gc, 0))
  FROM projected n
  JOIN unprojected o ON o.tenant_id = n.tenant_id AND o.event_id = n.event_id
  JOIN ledger.journal_entry e ON e.tenant_id = n.tenant_id AND e.fiscal_year = n.fiscal_year AND e.id = n.journal_entry_id
  JOIN ledger.journal_line l ON l.tenant_id = n.tenant_id AND l.fiscal_year = n.fiscal_year AND l.journal_entry_id = n.journal_entry_id
  WHERE o.projected_at IS NULL AND n.projected_at IS NOT NULL
    AND (l.cost_center_id IS NOT NULL OR l.profit_center_id IS NOT NULL OR l.segment_id IS NOT NULL)
  GROUP BY 1, 2, 3, 4, 5, 6, 7, 8, 9, 10 ORDER BY 1, 2, 3, 4, 5, 6, 7, 8, 9, 10
  ON CONFLICT (tenant_id, id) DO UPDATE SET
    debit_tc = ab.debit_tc + EXCLUDED.debit_tc, credit_tc = ab.credit_tc + EXCLUDED.credit_tc,
    debit_lc = ab.debit_lc + EXCLUDED.debit_lc, credit_lc = ab.credit_lc + EXCLUDED.credit_lc,
    debit_gc = ab.debit_gc + EXCLUDED.debit_gc, credit_gc = ab.credit_gc + EXCLUDED.credit_gc, updated_at = now();
  RETURN NULL;
END $$;
-- Creating the trigger waits for projector rounds under way and holds off
-- new ones until this migration commits; the backfill below then reads
-- every entry projected without it, and every later projection fires it.
CREATE TRIGGER balance_projection_assignment AFTER UPDATE ON ledger.balance_projection
  REFERENCING OLD TABLE AS unprojected NEW TABLE AS projected FOR EACH STATEMENT EXECUTE FUNCTION ledger.project_assignment_balance();

-- The balances of the lines projected so far. Lines carry centers only
-- since T56d (000155), and center accounting is turned on from a fiscal
-- year with no postings yet (T56c), so these are the lines of at most the
-- open year since T56d's release, posted while postings wait for this
-- statement. Were that a long backfill (06 §6.4), it would be an operation
-- in batches instead: an open question for the product owner (ADR-0050 §7
-- as built). Tenant rows: under a role that does not bypass row-level
-- security this fails rather than project nothing, as 000054.
SET LOCAL row_security = off;
INSERT INTO ledger.assignment_balance (tenant_id, ledger_id, legal_entity_id, account_id, cost_center_id, profit_center_id, segment_id,
  fiscal_year, fiscal_period, currency_tc, debit_tc, credit_tc, debit_lc, credit_lc, debit_gc, credit_gc)
SELECT l.tenant_id, l.ledger_id, l.legal_entity_id, l.account_id, l.cost_center_id, l.profit_center_id, l.segment_id, l.fiscal_year, l.fiscal_period,
  e.currency_tc, sum(greatest(l.amount_tc, 0)), sum(greatest(-l.amount_tc, 0)), sum(greatest(l.amount_lc, 0)), sum(greatest(-l.amount_lc, 0)),
  sum(greatest(l.amount_gc, 0)), sum(greatest(-l.amount_gc, 0))
FROM ledger.balance_projection q
JOIN ledger.journal_entry e ON e.tenant_id = q.tenant_id AND e.fiscal_year = q.fiscal_year AND e.id = q.journal_entry_id
JOIN ledger.journal_line l ON l.tenant_id = q.tenant_id AND l.fiscal_year = q.fiscal_year AND l.journal_entry_id = q.journal_entry_id
WHERE q.projected_at IS NOT NULL AND (l.cost_center_id IS NOT NULL OR l.profit_center_id IS NOT NULL OR l.segment_id IS NOT NULL)
GROUP BY 1, 2, 3, 4, 5, 6, 7, 8, 9, 10;
