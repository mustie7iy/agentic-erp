-- A legal entity's local currency never changes once it is created (F14,
-- 03-data-model §3.5, T31): configurations are checked against it
-- (companies[].currency must equal it) and its postings and balances are
-- kept in it, so changing it would leave both meaning another currency.
-- The application never changes it (tenancy updates name and extensions
-- only); the database now holds the rule too. A legal entity in another
-- currency is a new legal entity.
CREATE FUNCTION tenancy.check_local_currency() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'local currency of legal entity % is %; it cannot change', OLD.code, OLD.local_currency USING ERRCODE = 'check_violation';
END $$;
ALTER FUNCTION tenancy.check_local_currency() OWNER TO erp_owner;
CREATE TRIGGER legal_entity_local_currency_fixed BEFORE UPDATE OF local_currency ON tenancy.legal_entity
  FOR EACH ROW WHEN (NEW.local_currency IS DISTINCT FROM OLD.local_currency) EXECUTE FUNCTION tenancy.check_local_currency();
