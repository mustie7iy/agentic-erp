-- 000147's items, without jurisdictions on lines.
CREATE OR REPLACE FUNCTION ledger.write_tax_items(t uuid, y smallint, e uuid, day date, former boolean) RETURNS integer
LANGUAGE sql AS $$
  WITH lines AS (
    SELECT l.legal_entity_id, l.account_id, l.tax_code, l.amount_tc, l.amount_lc, l.tax_base_tc, l.tax_base_lc FROM ledger.journal_line l
    WHERE l.tenant_id = t AND l.fiscal_year = y AND l.journal_entry_id = e AND l.tax_code IS NOT NULL
  ), codes AS (
    SELECT DISTINCT legal_entity_id, tax_code FROM lines
  ), accounts AS (  -- every account a component of a code posts its tax to
    SELECT c.code, a FROM ledger.tax_component c JOIN codes ON codes.legal_entity_id = c.legal_entity_id AND codes.tax_code = c.code
      CROSS JOIN LATERAL unnest(CASE WHEN former THEN c.former_account_ids || c.account_id ELSE ARRAY[c.account_id] END) a
    WHERE c.tenant_id = t AND a IS NOT NULL
  ), base AS (
    SELECT l.tax_code, sum(l.amount_tc)::bigint tc, sum(l.amount_lc)::bigint lc FROM lines l
    WHERE NOT EXISTS (SELECT FROM accounts x WHERE x.code = l.tax_code AND x.a = l.account_id) GROUP BY 1
  ), comps AS (
    SELECT c.legal_entity_id, c.code, c.component, c.jurisdiction, c.account_key, c.recoverable_pct, c.account_id, c.former_account_ids,
      d.direction, d.kind, r.id rate_id, r.rate_pct
    FROM ledger.tax_component c JOIN codes ON codes.legal_entity_id = c.legal_entity_id AND codes.tax_code = c.code
    JOIN ledger.tax_code_definition d ON d.tenant_id = c.tenant_id AND d.legal_entity_id = c.legal_entity_id AND d.code = c.code
    JOIN ledger.tax_rate r ON r.tenant_id = c.tenant_id AND r.legal_entity_id = c.legal_entity_id AND r.code = c.code AND r.component = c.component
      AND r.withdrawn_at IS NULL AND r.valid_from <= day AND (r.valid_to IS NULL OR r.valid_to > day)
    WHERE c.tenant_id = t AND c.account_id IS NOT NULL
  ), taxed AS (  -- each component's tax per account it has lines on, or its account with none
    SELECT c.*, coalesce(s.account_id, c.account_id) tax_account, coalesce(s.tc, 0) tax_tc, coalesce(s.lc, 0) tax_lc,
      coalesce(s.btc, 0) declared_tc, coalesce(s.blc, 0) declared_lc FROM comps c
    LEFT JOIN LATERAL (SELECT l.account_id, sum(l.amount_tc)::bigint tc, sum(l.amount_lc)::bigint lc, sum(l.tax_base_tc)::bigint btc,
        sum(l.tax_base_lc)::bigint blc FROM lines l
      WHERE l.tax_code = c.code AND (l.account_id = c.account_id OR (former AND l.account_id = ANY(c.former_account_ids))) GROUP BY 1) s ON true
  ), ins AS (
    INSERT INTO ledger.tax_item (tenant_id, fiscal_year, journal_entry_id, item_no, legal_entity_id, code, component, jurisdiction, direction, kind,
      account_key, account_id, tax_date, tax_rate_id, rate_pct, recoverable_pct, base_tc, tax_tc, recoverable_tc, base_lc, tax_lc, recoverable_lc)
    SELECT t, y, e, row_number() OVER (ORDER BY x.code, x.component, x.tax_account), x.legal_entity_id, x.code, x.component, x.jurisdiction,
      x.direction, x.kind, x.account_key, x.tax_account, day, x.rate_id, x.rate_pct, x.recoverable_pct,
      -- The code's base once per component, on its first account's item.
      CASE WHEN row_number() OVER (PARTITION BY x.code, x.component ORDER BY x.tax_account) = 1 THEN coalesce(b.tc, 0) ELSE 0 END + x.declared_tc, x.tax_tc,
      CASE WHEN x.account_key IN ('output', 'self_output') THEN 0 ELSE x.tax_tc END,
      CASE WHEN row_number() OVER (PARTITION BY x.code, x.component ORDER BY x.tax_account) = 1 THEN coalesce(b.lc, 0) ELSE 0 END + x.declared_lc, x.tax_lc,
      CASE WHEN x.account_key IN ('output', 'self_output') THEN 0 ELSE x.tax_lc END
    FROM taxed x LEFT JOIN base b ON b.tax_code = x.code
    RETURNING 1
  )
  SELECT count(*)::integer FROM ins
$$;
ALTER FUNCTION ledger.write_tax_items(uuid, smallint, uuid, date, boolean) OWNER TO erp_owner;
DROP TRIGGER journal_line_tax_jurisdiction_code ON ledger.journal_line;
DROP FUNCTION ledger.check_line_jurisdiction();
ALTER TABLE ledger.parked_entry_line DROP COLUMN tax_jurisdiction;
ALTER TABLE ledger.journal_line DROP COLUMN tax_jurisdiction;
ALTER TABLE ledger.tax_component DROP COLUMN jurisdiction_on_line;
