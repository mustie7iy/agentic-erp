-- Intercompany billings the previous version cannot hold are refused
-- rather than dropped (06 §6.4: a down migration loses no data).
SET LOCAL row_security = off; -- the guard reads every tenant's rows: a role held to row-level security fails here, not on zero rows (06 §6.4)
DO $$
BEGIN
  IF EXISTS (SELECT FROM sales.intercompany_billing) THEN
    RAISE EXCEPTION 'intercompany billings exist; the previous version cannot hold them' USING ERRCODE = 'check_violation';
  END IF;
END $$;
DROP TABLE sales.intercompany_billing_item;
DROP TABLE sales.intercompany_billing;
