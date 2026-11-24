-- An account determination key a company's configuration stops naming is
-- removed (ADR-0048 As built batch 93; 03-data-model §3.7): until now
-- ledger.Setup only inserted and updated the keys the document names, so
-- a company that dropped cash_discount_granted or supplier_rebate kept
-- posting to the old account instead of falling back to the key it shared
-- (ledger.Accounts.Determination).
--
-- The application deletes from no tenant table (06 §6.4, TestDesignRules),
-- so a removal is recorded, insert-only, in
-- ledger.account_determination_removal: the key, the account it named,
-- when and by whom. Recording it deletes the determination row
-- (account_determination_removal_deletes, as the owner, as 000364's
-- clearing reset moves a clearing's items), and only a row naming that
-- very account: a removal of a key that is not determined, or names
-- another account, is refused. A removed key may be named again by a
-- later document, an ordinary insert, and removed again.
--
-- The keys that stay once activated are refused here too
-- (account_determination_removal_kept), as ledger.Setup.Check refuses them
-- on submission: gr_ir (the GR/IR positions are its lines; 000054's
-- account_determination_gr_ir_fixed), down_payment (its lines' reversals
-- post to it; 000078) and retained_earnings (ADR-0060 §4: changed, never
-- removed).
--
-- Expand only (06 §6.4): a new table and a trigger on it, which the
-- previous version never writes; a key removed by this version is a key
-- the previous version reads as not determined, as before it was set, and
-- names again with its own insert.
CREATE TABLE ledger.account_determination_removal (
  tenant_id        uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,
  key              text NOT NULL,
  removed_at       timestamptz NOT NULL DEFAULT now(),
  account_id       uuid NOT NULL,   -- the account the key named
  removed_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, legal_entity_id, key, removed_at),
  FOREIGN KEY (tenant_id, legal_entity_id, account_id) REFERENCES ledger.company_account (tenant_id, legal_entity_id, account_id),
  CONSTRAINT account_determination_removal_kept CHECK (key NOT IN ('gr_ir', 'down_payment', 'retained_earnings'))
);
CREATE TRIGGER account_determination_removal_immutable BEFORE UPDATE OR DELETE ON ledger.account_determination_removal
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('ledger.account_determination_removal'::regclass);

-- The removal deletes the row it names, as the owner, since the
-- application may not; row-level security still applies to the owner
-- (FORCE), so only the tenant's own row goes.
CREATE FUNCTION ledger.delete_removed_determination() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  DELETE FROM ledger.account_determination
  WHERE tenant_id = NEW.tenant_id AND legal_entity_id = NEW.legal_entity_id AND key = NEW.key AND account_id = NEW.account_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'key % of legal entity % does not name account %', NEW.key, NEW.legal_entity_id, NEW.account_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'account_determination_removal_determined';
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION ledger.delete_removed_determination() OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION ledger.delete_removed_determination() FROM PUBLIC;
CREATE TRIGGER account_determination_removal_deletes AFTER INSERT ON ledger.account_determination_removal
  FOR EACH ROW EXECUTE FUNCTION ledger.delete_removed_determination();
