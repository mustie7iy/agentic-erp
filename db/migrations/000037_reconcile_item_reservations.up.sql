-- Reconciles the ledger's item reservations (000034) with the payment runs
-- (F5, 03-data-model §3.10). An application version from before 000034,
-- running beside the new one during the rollout, rejects runs and settles
-- payments without ending their reservations, and proposes runs without
-- making any. This contract step ships in the release after 000034, once
-- no such version runs: reservations no live run holds are released, and
-- the items live runs hold are reserved.
UPDATE ledger.item_reservation ir SET released_at = now(), released_by = ir.created_by
WHERE ir.released_at IS NULL AND ir.owner_context = 'payments' AND NOT EXISTS (
  SELECT FROM payments.payment_item i
  JOIN payments.payment p ON p.tenant_id = i.tenant_id AND p.id = i.payment_id
  JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id
  WHERE i.tenant_id = ir.tenant_id AND r.id = ir.owner_id AND i.item_fiscal_year = ir.fiscal_year
    AND i.item_entry_id = ir.journal_entry_id AND i.item_line_no = ir.line_no AND i.is_active
    AND (r.status IN ('proposing', 'proposed') OR (r.status = 'releasing' AND p.journal_entry_id IS NULL)));
INSERT INTO ledger.item_reservation (tenant_id, owner_id, fiscal_year, journal_entry_id, line_no, owner_context, owner_reference, created_by)
SELECT i.tenant_id, r.id, i.item_fiscal_year, i.item_entry_id, i.item_line_no, 'payments', r.document_number, r.proposed_by
FROM payments.payment_item i
JOIN payments.payment p ON p.tenant_id = i.tenant_id AND p.id = i.payment_id
JOIN payments.payment_run r ON r.tenant_id = p.tenant_id AND r.id = p.payment_run_id
WHERE i.is_active AND (r.status IN ('proposing', 'proposed') OR (r.status = 'releasing' AND p.journal_entry_id IS NULL))
ON CONFLICT DO NOTHING;
