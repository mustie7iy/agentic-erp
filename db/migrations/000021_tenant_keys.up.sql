-- Key management (T14): every tenant's sensitive fields are sealed with the
-- tenant's own data key and fingerprinted with its own fingerprint key, both
-- stored wrapped by a master key of the deployment's ring. Rotating a tenant
-- retires its key, stores a new one and re-seals every sealed column in the
-- same transaction. Tenants without a row are still on the single
-- deployment key of format version 1, until their first rotation.
CREATE TABLE tenancy.tenant_key (
  tenant_id         uuid NOT NULL,
  id                uuid NOT NULL,
  master_key_id     text NOT NULL CHECK (length(master_key_id) BETWEEN 1 AND 64),
  wrapped_data_key  bytea NOT NULL,
  wrapped_mac_key   bytea NOT NULL,
  status            text NOT NULL CHECK (status IN ('active', 'retired')),
  created_at        timestamptz NOT NULL DEFAULT now(),
  retired_at        timestamptz,
  updated_at        timestamptz NOT NULL DEFAULT now(),
  version           integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  CHECK ((status = 'retired') = (retired_at IS NOT NULL))
);
CREATE UNIQUE INDEX tenant_key_one_active ON tenancy.tenant_key (tenant_id) WHERE status = 'active';
CREATE TRIGGER tenant_key_touch BEFORE UPDATE ON tenancy.tenant_key FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('tenancy.tenant_key'::regclass);

-- Re-sealing rewrites the cipher and fingerprint of effective-dated bank
-- accounts without touching their validity: the validity trigger lets an
-- update through when nothing but sealed columns changed.
CREATE OR REPLACE FUNCTION platform.end_validity() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  before jsonb;
  after jsonb;
BEGIN
  SELECT coalesce(jsonb_object_agg(key, value), '{}') INTO before FROM jsonb_each(to_jsonb(OLD)) WHERE key NOT LIKE '%\_cipher' AND key NOT LIKE '%\_fingerprint';
  SELECT coalesce(jsonb_object_agg(key, value), '{}') INTO after FROM jsonb_each(to_jsonb(NEW)) WHERE key NOT LIKE '%\_cipher' AND key NOT LIKE '%\_fingerprint';
  IF before = after THEN
    RETURN NEW; -- the same value sealed under another key
  END IF;
  IF NEW.valid_to IS NULL OR (OLD.valid_to IS NOT NULL AND NEW.valid_to >= OLD.valid_to) THEN
    RAISE EXCEPTION '%: validity can only be ended or shortened', TG_TABLE_NAME USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.valid_to < current_date THEN
    RAISE EXCEPTION '%: validity cannot be ended in the past', TG_TABLE_NAME USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.ended_by IS NULL THEN
    RAISE EXCEPTION '%: ended_by is required', TG_TABLE_NAME USING ERRCODE = 'not_null_violation';
  END IF;
  NEW.ended_at := now();
  RETURN NEW;
END $$;
GRANT UPDATE (account_cipher, account_fingerprint) ON partner.bank_account TO erp_app;
