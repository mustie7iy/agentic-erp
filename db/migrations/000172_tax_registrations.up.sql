-- The tax filing context (T61g; ADR-0049 §10, Alternatives; 03-data-model
-- §3.20) in its own schema, tax: the companies' registrations with tax
-- authorities, kept from the configuration's companies[].tax_registrations
-- by taxfiling.Setup. A registration's filing periods follow from its
-- frequency and first period's start, which never change under its code,
-- so they are computed on read and not stored; T61i stores a return per
-- registration and period. The registration number is sealed with the
-- tenant's key (fieldcrypt) and shown by its last four characters.
-- legal_entity_id and group_member_ids are the tenancy context's: no
-- foreign key crosses a schema. Expand only: a new schema, which the
-- previous version neither reads nor writes.
CREATE SCHEMA tax;
GRANT USAGE ON SCHEMA tax TO erp_app, erp_readonly, erp_owner;

CREATE TABLE tax.registration (
  tenant_id           uuid NOT NULL,
  id                  uuid NOT NULL,
  code                text NOT NULL CHECK (code ~ '^[A-Z0-9_]{1,20}$'),
  legal_entity_id     uuid NOT NULL,     -- tenancy context: the registrant (a tax group's representative member)
  group_member_ids    uuid[] NOT NULL DEFAULT '{}',  -- tenancy context: a tax group's other members
  authority           text NOT NULL CHECK (authority ~ '^[A-Z][A-Z0-9_]{0,19}$'),
  number_cipher       bytea NOT NULL,
  number_fingerprint  text NOT NULL CHECK (number_fingerprint ~ '^[0-9a-f]{64}$'),
  number_last4        text NOT NULL CHECK (length(number_last4) BETWEEN 1 AND 4),
  return_type         text NOT NULL CHECK (return_type ~ '^[A-Z0-9_-]{1,20}$'),
  frequency           text NOT NULL CHECK (frequency IN ('monthly', 'quarterly', 'annual')),
  first_period_start  date NOT NULL CHECK (extract(day FROM first_period_start) <= 28),
  last_period_end     date CHECK (last_period_end > first_period_start),
  is_active           boolean NOT NULL DEFAULT true,  -- false once the configuration no longer has it
  version             integer NOT NULL DEFAULT 1,
  created_at          timestamptz NOT NULL DEFAULT now(),
  created_by          uuid NOT NULL,
  updated_at          timestamptz NOT NULL DEFAULT now(),
  updated_by          uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT registration_code_once UNIQUE (tenant_id, code),
  CONSTRAINT registration_members CHECK (NOT legal_entity_id = ANY (group_member_ids))
);
CREATE INDEX registration_by_company ON tax.registration (tenant_id, legal_entity_id);
CREATE TRIGGER registration_touch BEFORE UPDATE ON tax.registration FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('tax.registration'::regclass);

-- What a registration's periods and returns follow from never changes
-- under its code: a changed frequency, start, authority, return, registrant
-- or group is a registration of a new code, the old one ended by its
-- last_period_end. The number is compared by the application, which opens
-- it (its fingerprint changes when the tenant's key is rotated).
CREATE FUNCTION tax.check_registration_fixed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.code, NEW.legal_entity_id, NEW.group_member_ids, NEW.authority, NEW.return_type, NEW.frequency, NEW.first_period_start)
     IS DISTINCT FROM (OLD.code, OLD.legal_entity_id, OLD.group_member_ids, OLD.authority, OLD.return_type, OLD.frequency, OLD.first_period_start) THEN
    RAISE EXCEPTION 'tax registration % keeps its authority, return, frequency, first period and companies', OLD.code
      USING ERRCODE = 'check_violation', CONSTRAINT = 'registration_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION tax.check_registration_fixed() OWNER TO erp_owner;
CREATE TRIGGER registration_fixed BEFORE UPDATE ON tax.registration FOR EACH ROW EXECUTE FUNCTION tax.check_registration_fixed();

INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('tax.registration.read', 'View tax registrations and their filing periods', false);
