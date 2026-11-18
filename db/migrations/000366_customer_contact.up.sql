-- Contacts with customers and resubmission dates (T76g; ADR-0065 As
-- built; 03-data-model §3.27): a collector records a contact with a
-- customer of a company (when, whom at the customer, the outcome, a note)
-- and optionally the date the customer is to be worked again. While a
-- customer's last contact (its latest by contacted_at) has a
-- resubmission date after a worklist's key date, the customer is not on
-- that worklist. customer_id is the partner context's, legal_entity_id
-- the tenancy context's: no foreign key leaves the schema (02 §2.1). A
-- contact is a record of what happened: insert-only, corrected by a
-- later one. Nothing is posted or sent.
-- Expand only (06 §6.4): a new table the previous version does not read.
CREATE TABLE collections.customer_contact (
  tenant_id          uuid NOT NULL,
  id                 uuid NOT NULL,
  legal_entity_id    uuid NOT NULL,     -- tenancy context
  customer_id        uuid NOT NULL,     -- partner context
  customer_number    text NOT NULL CONSTRAINT customer_contact_customer_number_check CHECK (length(customer_number) BETWEEN 1 AND 40),
  contacted_at       timestamptz NOT NULL,
  -- Whom at the customer the collector spoke to: a person's name (the personal data inventory names it).
  contact_name       text CONSTRAINT customer_contact_contact_name_check CHECK (btrim(contact_name) <> '' AND length(contact_name) <= 200),
  outcome            text NOT NULL CONSTRAINT customer_contact_outcome_check CHECK (outcome IN ('reached', 'not_reached', 'left_message')),
  note               text CONSTRAINT customer_contact_note_check CHECK (btrim(note) <> '' AND length(note) <= 2000),
  resubmission_date  date CONSTRAINT customer_contact_resubmission_date_check CHECK (resubmission_date <= '2100-12-31'),
  version            integer NOT NULL DEFAULT 1 CONSTRAINT customer_contact_version_check CHECK (version >= 1),
  created_at         timestamptz NOT NULL DEFAULT now(),
  created_by         uuid NOT NULL,
  updated_at         timestamptz NOT NULL DEFAULT now(),
  updated_by         uuid NOT NULL,
  PRIMARY KEY (tenant_id, id)
);
CREATE TRIGGER customer_contact_insert_only BEFORE UPDATE OR DELETE ON collections.customer_contact
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
-- A customer's last contact (the worklist's rows, and the customers it leaves out).
CREATE INDEX customer_contact_latest ON collections.customer_contact (tenant_id, legal_entity_id, customer_id, contacted_at DESC, id DESC);
-- The customers whose contacts set a resubmission date, which a worklist checks for being the last.
CREATE INDEX customer_contact_resubmission ON collections.customer_contact (tenant_id, legal_entity_id, resubmission_date)
  WHERE resubmission_date IS NOT NULL;
-- The list: the tenant's contacts, the latest first, in the order its pages are read.
CREATE INDEX customer_contact_by_contacted_at ON collections.customer_contact (tenant_id, contacted_at, id);
SELECT platform.secure_table('collections.customer_contact'::regclass);
