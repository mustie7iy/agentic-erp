DROP TABLE controlling.order_budget_change;
ALTER TABLE controlling.order_budget DROP CONSTRAINT order_budget_warn_below_refuse,
  DROP COLUMN refuse_percent, DROP COLUMN warn_percent;
