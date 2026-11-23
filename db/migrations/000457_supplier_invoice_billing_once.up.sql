-- migrate:no-transaction
-- An intercompany billing is entered as one live supplier invoice (S2i3;
-- ADR-0082 As built S2i3): the key decides between two requests racing
-- for one billing. A rejected, cancelled or reversed invoice frees the
-- billing for a corrected entry, as supplier_invoice_no_duplicates frees
-- its reference. Built concurrently (06 §6.4).
CREATE UNIQUE INDEX CONCURRENTLY supplier_invoice_billing_once ON payables.supplier_invoice (tenant_id, intercompany_billing_id)
  WHERE intercompany_billing_id IS NOT NULL AND status NOT IN ('rejected', 'cancelled', 'reversed');
