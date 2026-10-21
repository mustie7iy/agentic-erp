CREATE OR REPLACE FUNCTION ledger.tax_code_posted(t uuid, le uuid, c text, lo date, hi date) RETURNS boolean
LANGUAGE sql STABLE AS $$
  SELECT lo < coalesce(hi, 'infinity'::date) AND EXISTS (SELECT FROM ledger.journal_line l WHERE l.tenant_id = t AND l.legal_entity_id = le
    AND l.tax_code = c AND l.posting_date >= lo AND (hi IS NULL OR l.posting_date < hi))
$$;
DROP TRIGGER journal_entry_tax_items ON ledger.journal_entry;
DROP TABLE ledger.tax_item;
DROP FUNCTION ledger.check_entry_tax_items();
DROP FUNCTION ledger.check_tax_items(uuid, smallint, uuid);
DROP FUNCTION ledger.write_tax_items(uuid, smallint, uuid, date, boolean);
DROP FUNCTION ledger.check_tax_item_fixed();
ALTER TABLE ledger.parked_entry DROP COLUMN tax_date;
ALTER TABLE ledger.journal_entry DROP COLUMN tax_date;
