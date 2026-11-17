-- Budget supplements and returns, and availability control (T60h;
-- ADR-0064 §3, plan-depth-1 §T60h).
--
-- An order's budget gains an availability control: a warning limit and a
-- refusal limit, each a whole percentage of the budget, either optional.
-- The ledger asks controlling (ledger.CostObjects.Consume) for every
-- posting that adds to an order's consumption; above the refusal limit the
-- posting is refused, above the warning limit it is published as a
-- warning. A warning limit at or above the refusal limit would never be
-- reached, so the database refuses it.
--
-- Every change of a budget from here on is kept as one row of
-- controlling.order_budget_change, the version it made: a budget set or
-- its limits changed (set), supplemented or returned. Insert-only: the
-- table has no version column of its own (budget_version is the budget's),
-- so no UPDATE is granted, and never a DELETE. Budgets set before this migration have no
-- row for their earlier versions: the amounts they replaced were never
-- kept.
--
-- Expand only: two nullable columns and a new table, which the previous
-- version neither reads nor writes.
ALTER TABLE controlling.order_budget
  ADD COLUMN warn_percent smallint CONSTRAINT order_budget_warn_percent CHECK (warn_percent BETWEEN 1 AND 999),
  ADD COLUMN refuse_percent smallint CONSTRAINT order_budget_refuse_percent CHECK (refuse_percent BETWEEN 1 AND 999),
  ADD CONSTRAINT order_budget_warn_below_refuse CHECK (warn_percent < refuse_percent);

CREATE TABLE controlling.order_budget_change (
  tenant_id          uuid NOT NULL,
  id                 uuid NOT NULL,
  internal_order_id  uuid NOT NULL,
  budget_version     integer NOT NULL CHECK (budget_version > 0),  -- the budget's version the change made
  kind               text NOT NULL CONSTRAINT order_budget_change_kind CHECK (kind IN ('set', 'supplement', 'return')),
  budget_before      bigint,                                 -- minor units; NULL when the order had no budget
  budget_after       bigint NOT NULL CONSTRAINT order_budget_change_not_negative CHECK (budget_after >= 0),
  warn_percent       smallint CHECK (warn_percent BETWEEN 1 AND 999),
  refuse_percent     smallint CHECK (refuse_percent BETWEEN 1 AND 999),
  text               text CONSTRAINT order_budget_change_text CHECK (char_length(text) BETWEEN 1 AND 200),
  created_at         timestamptz NOT NULL DEFAULT now(),
  created_by         uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT order_budget_change_order FOREIGN KEY (tenant_id, internal_order_id) REFERENCES controlling.internal_order (tenant_id, id),
  -- One row per version of a budget.
  CONSTRAINT order_budget_change_version_once UNIQUE (tenant_id, internal_order_id, budget_version),
  -- A supplement raises a budget that exists, a return lowers one.
  CONSTRAINT order_budget_change_direction CHECK (kind NOT IN ('supplement', 'return')
    OR (kind = 'supplement' AND budget_before IS NOT NULL AND budget_after > budget_before)
    OR (kind = 'return' AND budget_before IS NOT NULL AND budget_after < budget_before))
);
SELECT platform.secure_table('controlling.order_budget_change'::regclass);
