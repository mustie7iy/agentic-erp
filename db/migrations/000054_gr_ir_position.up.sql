-- GR/IR positions (T33f, 03-data-model §3.7). Each order item's GR/IR
-- position, the sum of its lines on the legal entity's GR/IR account, is
-- projected into a row the ledger updates in the transaction that posts
-- the lines, under the order's GR/IR lock: the positions are read without
-- summing the history, and GET /v1/gr-ir-balances pages the current ones
-- by an index range scan (04-api-design §4.5). GR/IR lines are posted in
-- the leading ledger only (ledger.Postings), so a position has no ledger.
-- received is the procurement context's quantity negated, invoiced the
-- payables context's; the balance is in the three currencies of the lines.
CREATE TABLE ledger.gr_ir_position (
  tenant_id          uuid NOT NULL,
  legal_entity_id    uuid NOT NULL,
  purchase_order_id  uuid NOT NULL,                -- procurement context
  po_item_no         integer NOT NULL,
  id                 uuid GENERATED ALWAYS AS (md5(purchase_order_id::text || '/' || po_item_no::text)::uuid) STORED,
  account_id         uuid NOT NULL,
  currency_tc        char(3) NOT NULL REFERENCES ref.currency,
  currency_lc        char(3) NOT NULL REFERENCES ref.currency,
  currency_gc        char(3) NOT NULL REFERENCES ref.currency,
  uom_code           text NOT NULL REFERENCES ref.unit_of_measure,
  received           numeric(20,6) NOT NULL,
  invoiced           numeric(20,6) NOT NULL,
  amount_tc          bigint NOT NULL,
  amount_lc          bigint NOT NULL,
  amount_gc          bigint NOT NULL,
  last_posting_date  date NOT NULL,
  is_open            boolean GENERATED ALWAYS AS (received <> invoiced OR amount_tc <> 0 OR amount_lc <> 0 OR amount_gc <> 0) STORED,
  updated_at         timestamptz NOT NULL DEFAULT now(),
  version            integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, legal_entity_id, purchase_order_id, po_item_no),
  FOREIGN KEY (tenant_id, legal_entity_id, account_id) REFERENCES ledger.company_account (tenant_id, legal_entity_id, account_id)
);
-- The listing's keyset orders, for open positions (the default) and all.
CREATE INDEX gr_ir_position_open_by_order ON ledger.gr_ir_position (tenant_id, legal_entity_id, purchase_order_id, id) WHERE is_open;
CREATE INDEX gr_ir_position_open_by_date ON ledger.gr_ir_position (tenant_id, legal_entity_id, last_posting_date, id) WHERE is_open;
CREATE INDEX gr_ir_position_by_order ON ledger.gr_ir_position (tenant_id, legal_entity_id, purchase_order_id, id);
CREATE INDEX gr_ir_position_by_date ON ledger.gr_ir_position (tenant_id, legal_entity_id, last_posting_date, id);
CREATE TRIGGER gr_ir_position_touch BEFORE UPDATE ON ledger.gr_ir_position FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.gr_ir_position'::regclass);

-- The positions the lines posted so far add up to. Tenant rows: under a
-- role that does not bypass row-level security this fails rather than
-- project nothing (06-engineering-process §6.4).
SET LOCAL row_security = off;
INSERT INTO ledger.gr_ir_position (tenant_id, legal_entity_id, purchase_order_id, po_item_no, account_id, currency_tc, currency_lc, currency_gc,
  uom_code, received, invoiced, amount_tc, amount_lc, amount_gc, last_posting_date)
SELECT l.tenant_id, l.legal_entity_id, l.purchase_order_id, l.po_item_no, l.account_id, min(e.currency_tc), min(e.currency_lc), min(e.currency_gc),
  min(l.uom_code), coalesce(-sum(l.quantity) FILTER (WHERE e.source_context = 'procurement'), 0),
  coalesce(sum(l.quantity) FILTER (WHERE e.source_context = 'payables'), 0), sum(l.amount_tc), sum(l.amount_lc), sum(l.amount_gc), max(l.posting_date)
FROM ledger.journal_line l
JOIN ledger.account_determination d ON d.tenant_id = l.tenant_id AND d.legal_entity_id = l.legal_entity_id AND d.key = 'gr_ir' AND d.account_id = l.account_id
JOIN ledger.journal_entry e ON e.tenant_id = l.tenant_id AND e.fiscal_year = l.fiscal_year AND e.id = l.journal_entry_id
WHERE l.purchase_order_id IS NOT NULL
GROUP BY l.tenant_id, l.legal_entity_id, l.purchase_order_id, l.po_item_no, l.account_id;
