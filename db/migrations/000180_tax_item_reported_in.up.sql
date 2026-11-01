-- migrate:no-transaction
-- A return's items (T61i, ADR-0049 §11): what a withdrawal releases and
-- tax.check_item_reported reads at commit. Built concurrently, as
-- ledger.tax_item is large and posted to while the release rolls out
-- (06-engineering-process §6.4). A failed build leaves an invalid index,
-- dropped by hand before the retry.
CREATE INDEX CONCURRENTLY tax_item_reported_in ON ledger.tax_item (tenant_id, reported_in) WHERE reported_in IS NOT NULL;
