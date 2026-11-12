-- The settlement rule of an internal order (T60d; ADR-0064 §3,
-- plan-breadth-week1 §T60d): one receiver per order, a cost center of the
-- order's company, taking the whole of what the order is settled for
-- (T60e). The receiver is the ledger's: no foreign key crosses a schema;
-- the controlling context checks it through ledger.AssignmentBalances
-- when the rule is set. An order without a row has no rule. The rule is
-- replaced with the order's version, which moves with it (ADR-0008): the
-- row's own version only counts its replacements, as every updatable
-- table's does (platform.touch). Never deleted. Expand only: a new table,
-- which the previous version neither reads nor writes.
CREATE TABLE controlling.settlement_rule (
  tenant_id                uuid NOT NULL,
  internal_order_id        uuid NOT NULL,
  receiver_cost_center_id  uuid NOT NULL,     -- ledger context
  version                  integer NOT NULL DEFAULT 1,
  created_at               timestamptz NOT NULL DEFAULT now(),
  created_by               uuid NOT NULL,
  updated_at               timestamptz NOT NULL DEFAULT now(),
  updated_by               uuid NOT NULL,
  PRIMARY KEY (tenant_id, internal_order_id),
  CONSTRAINT settlement_rule_order FOREIGN KEY (tenant_id, internal_order_id) REFERENCES controlling.internal_order (tenant_id, id)
);
CREATE TRIGGER settlement_rule_touch BEFORE UPDATE ON controlling.settlement_rule FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('controlling.settlement_rule'::regclass);
