-- The budget of an internal order (T60c; ADR-0064 §3, plan-breadth-week1
-- §T60c): one overall amount per order, in integer minor units of the
-- currency of the order's company (ADR-0005), which a company keeps
-- (000045). An order without a row has no budget. The budget is replaced
-- with the version its reader saw (ADR-0008) and never deleted; nothing is
-- refused for it: the read compares it with the order's actual costs.
-- Expand only: a new table, which the previous version neither reads nor
-- writes.
CREATE TABLE controlling.order_budget (
  tenant_id          uuid NOT NULL,
  internal_order_id  uuid NOT NULL,
  amount             bigint NOT NULL CONSTRAINT order_budget_not_negative CHECK (amount >= 0),
  version            integer NOT NULL DEFAULT 1,
  created_at         timestamptz NOT NULL DEFAULT now(),
  created_by         uuid NOT NULL,
  updated_at         timestamptz NOT NULL DEFAULT now(),
  updated_by         uuid NOT NULL,
  PRIMARY KEY (tenant_id, internal_order_id),
  CONSTRAINT order_budget_order FOREIGN KEY (tenant_id, internal_order_id) REFERENCES controlling.internal_order (tenant_id, id)
);
CREATE TRIGGER order_budget_touch BEFORE UPDATE ON controlling.order_budget FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('controlling.order_budget'::regclass);

-- Setting an order's budget (ADR-0064 §3): a person's, by permission, with
-- no approval request.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('controlling.order.budget', 'Set the budgets of internal orders', false);
