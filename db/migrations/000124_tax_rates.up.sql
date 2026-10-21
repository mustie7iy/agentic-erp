-- Dated tax codes with components (T61a, ADR-0049 §3). A tax code is its
-- definition (direction, kind, UN/CEFACT category), its components (SAP's
-- condition types: an account key, a jurisdiction, a recoverable share and
-- an account) and each component's dated rates. The configuration names
-- every rate with its dates, so a known change is set up ahead; a rate a
-- posting used is never edited or withdrawn, and no rate is added over
-- dates already posted with its code, so the past is never re-rated.
--
-- Expand and migrate (06-engineering-process §6.4): ledger.tax_code, one
-- row per code and validity with one rate and account, is copied here and
-- no longer read or written by this release; a later release drops it
-- (contract, tracked with T61c). A replica of the previous release reads
-- it during the rollout and sees the codes as they were before this
-- migration, so tax code changes wait until the rollout completes, as
-- configuration changes with new keys do.

CREATE TABLE ledger.tax_code_definition (
  tenant_id        uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,
  code             text NOT NULL CHECK (code ~ '^[A-Z0-9]{1,4}$'),
  description      text NOT NULL,
  direction        text NOT NULL CHECK (direction IN ('input', 'output')),
  kind             text NOT NULL CHECK (kind IN ('standard', 'zero', 'exempt', 'out_of_scope', 'reverse_charge', 'import')),
  category         text CHECK (category IN ('S', 'Z', 'E', 'AE', 'K', 'G', 'O', 'L', 'M', 'B')),  -- UN/CEFACT 5305, for e-invoicing (T65)
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, legal_entity_id, code)
);
CREATE TRIGGER tax_code_definition_touch BEFORE UPDATE ON ledger.tax_code_definition FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.tax_code_definition'::regclass);
-- A code's direction is what its postings meant; it never changes.
REVOKE UPDATE ON ledger.tax_code_definition FROM erp_app;
GRANT UPDATE (description, kind, category, version, updated_by) ON ledger.tax_code_definition TO erp_app;

CREATE TABLE ledger.tax_component (
  tenant_id           uuid NOT NULL,
  legal_entity_id     uuid NOT NULL,
  code                text NOT NULL,
  component           text NOT NULL CHECK (component ~ '^[a-z][a-z0-9_]{0,19}$'),
  jurisdiction        text CHECK (jurisdiction ~ '^[A-Z]{2}(-[A-Z0-9]{1,3})?$'),  -- ISO 3166-1 or 3166-2: CA, CA-BC, AE-DU
  -- SAP's account keys: MWS output, VST input, NVV non-deductible (onto the
  -- base line, no account of its own), ESA/ESE self-assessed output/input.
  account_key         text NOT NULL CHECK (account_key IN ('input', 'output', 'non_deductible', 'self_output', 'self_input')),
  recoverable_pct     numeric(6,3) NOT NULL CHECK (recoverable_pct >= 0 AND recoverable_pct <= 100),
  account_id          uuid,
  -- The accounts the component posted to before its current one: their
  -- lines are still its tax lines (a cash discount corrects them, T36d).
  -- Kept by tax_component_account_history, never written directly.
  former_account_ids  uuid[] NOT NULL DEFAULT '{}',
  version             integer NOT NULL DEFAULT 1,
  created_at          timestamptz NOT NULL DEFAULT now(),
  created_by          uuid NOT NULL,
  updated_at          timestamptz NOT NULL DEFAULT now(),
  updated_by          uuid NOT NULL,
  PRIMARY KEY (tenant_id, legal_entity_id, code, component),
  FOREIGN KEY (tenant_id, legal_entity_id, code) REFERENCES ledger.tax_code_definition (tenant_id, legal_entity_id, code),
  FOREIGN KEY (tenant_id, legal_entity_id, account_id) REFERENCES ledger.company_account (tenant_id, legal_entity_id, account_id),
  CONSTRAINT tax_component_account CHECK ((account_key = 'non_deductible') = (account_id IS NULL)),
  CONSTRAINT tax_component_non_deductible CHECK (account_key <> 'non_deductible' OR recoverable_pct = 0)
);
CREATE FUNCTION ledger.tax_component_account_history() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.account_id IS DISTINCT FROM NEW.account_id AND OLD.account_id IS NOT NULL
     AND NOT OLD.account_id = ANY(OLD.former_account_ids) THEN
    NEW.former_account_ids := OLD.former_account_ids || OLD.account_id;
  ELSE
    NEW.former_account_ids := OLD.former_account_ids;
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER tax_component_account_history BEFORE UPDATE ON ledger.tax_component FOR EACH ROW EXECUTE FUNCTION ledger.tax_component_account_history();
CREATE TRIGGER tax_component_touch BEFORE UPDATE ON ledger.tax_component FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.tax_component'::regclass);
-- The account key decides what a component's postings were; it never
-- changes. The account may: the history keeps the former ones.
REVOKE UPDATE ON ledger.tax_component FROM erp_app;
GRANT UPDATE (jurisdiction, recoverable_pct, account_id, version, updated_by) ON ledger.tax_component TO erp_app;

CREATE TABLE ledger.tax_rate (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,
  code             text NOT NULL,
  component        text NOT NULL,
  valid_from       date NOT NULL,
  valid_to         date,                 -- exclusive; open when null
  rate_pct         numeric(9,6) NOT NULL CHECK (rate_pct >= 0 AND rate_pct < 100),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  ended_at         timestamptz,          -- the last change of valid_to
  ended_by         uuid,
  withdrawn_at     timestamptz,          -- taken back before any posting used it
  withdrawn_by     uuid,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, legal_entity_id, code, component) REFERENCES ledger.tax_component (tenant_id, legal_entity_id, code, component),
  CHECK (valid_to IS NULL OR valid_to > valid_from),
  CONSTRAINT tax_rate_ended CHECK ((ended_at IS NULL) = (ended_by IS NULL)),
  CONSTRAINT tax_rate_withdrawn CHECK ((withdrawn_at IS NULL) = (withdrawn_by IS NULL)),
  CONSTRAINT tax_rate_no_overlap EXCLUDE USING gist (tenant_id WITH =, legal_entity_id WITH =, code WITH =, component WITH =,
    daterange(valid_from, valid_to) WITH &&) WHERE (withdrawn_at IS NULL)
);
CREATE INDEX tax_rate_by_code ON ledger.tax_rate (tenant_id, legal_entity_id, code, valid_from) WHERE withdrawn_at IS NULL;
SELECT platform.secure_table('ledger.tax_rate'::regclass);
-- A rate's value and start never change: a different rate is a new row.
GRANT UPDATE (valid_to, ended_at, ended_by, withdrawn_at, withdrawn_by) ON ledger.tax_rate TO erp_app;

-- Whether a posting of the code in the legal entity falls in [lo, hi) (hi
-- null: open). Postings are journal lines carrying the code, by posting
-- date, the date their tax codes are validated on; T61c's tax items, which
-- name the rate they applied, replace this.
CREATE FUNCTION ledger.tax_code_posted(t uuid, le uuid, c text, lo date, hi date) RETURNS boolean
LANGUAGE sql STABLE AS $$
  SELECT lo < coalesce(hi, 'infinity'::date) AND EXISTS (SELECT FROM ledger.journal_line l WHERE l.tenant_id = t AND l.legal_entity_id = le
    AND l.tax_code = c AND l.posting_date >= lo AND (hi IS NULL OR l.posting_date < hi))
$$;

-- A rate is used once a posting of its code falls in its dates. It is then
-- never withdrawn, and its dates never change in a way that moves a posting
-- in or out of them; a new rate never covers a posted date; a withdrawn
-- rate never changes again. Every posting holds its codes' definitions FOR
-- KEY SHARE and the setup locks them FOR UPDATE before changing a rate
-- (ledger.lockTaxCodes, taxCodes), so a change waits for a posting in
-- flight and then sees it.
CREATE FUNCTION ledger.check_tax_rate() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.withdrawn_at IS NULL AND ledger.tax_code_posted(NEW.tenant_id, NEW.legal_entity_id, NEW.code, NEW.valid_from, NEW.valid_to) THEN
      RAISE EXCEPTION 'tax code % has postings from % that a new rate of % would re-rate', NEW.code, NEW.valid_from, NEW.component
        USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_rate_in_use';
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.withdrawn_at IS NOT NULL THEN
    RAISE EXCEPTION 'a withdrawn rate of tax code % never changes', OLD.code USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_rate_withdrawn_fixed';
  END IF;
  IF NEW.withdrawn_at IS NOT NULL THEN
    IF ledger.tax_code_posted(OLD.tenant_id, OLD.legal_entity_id, OLD.code, OLD.valid_from, OLD.valid_to) THEN
      RAISE EXCEPTION 'the rate of tax code % from % is used by postings', OLD.code, OLD.valid_from
        USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_rate_in_use';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.valid_to IS DISTINCT FROM OLD.valid_to THEN
    IF ledger.tax_code_posted(OLD.tenant_id, OLD.legal_entity_id, OLD.code, least(OLD.valid_to, NEW.valid_to),
        CASE WHEN OLD.valid_to IS NULL OR NEW.valid_to IS NULL THEN NULL ELSE greatest(OLD.valid_to, NEW.valid_to) END) THEN
      RAISE EXCEPTION 'the rate of tax code % from % is used by postings between its old and new end', OLD.code, OLD.valid_from
        USING ERRCODE = 'check_violation', CONSTRAINT = 'tax_rate_in_use';
    END IF;
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER tax_rate_check BEFORE INSERT OR UPDATE ON ledger.tax_rate FOR EACH ROW EXECUTE FUNCTION ledger.check_tax_rate();

-- The usage check finds a code's postings by date. Built in a transaction,
-- not concurrently, for the reasons 000055 gives (a partitioned table;
-- before the pilot the journal holds test and demo data).
CREATE INDEX journal_line_tax_code ON ledger.journal_line (tenant_id, legal_entity_id, tax_code, posting_date) WHERE tax_code IS NOT NULL;

-- Migrate: each code becomes a definition with one component, "tax", whose
-- rates are the code's validity rows; the component's account is its
-- latest row's, the earlier ones its former accounts. Tenant rows: under a
-- role that does not bypass row-level security this fails rather than copy
-- nothing (06-engineering-process §6.4). The trigger is off while copying:
-- the rows being copied are the ones the postings used.
SET LOCAL row_security = off;
ALTER TABLE ledger.tax_rate DISABLE TRIGGER tax_rate_check;
INSERT INTO ledger.tax_code_definition (tenant_id, legal_entity_id, code, description, direction, kind, created_at, created_by, updated_by)
SELECT DISTINCT ON (tenant_id, legal_entity_id, code) tenant_id, legal_entity_id, code, description, direction, 'standard', created_at, created_by, created_by
FROM ledger.tax_code ORDER BY tenant_id, legal_entity_id, code, valid_from DESC;
INSERT INTO ledger.tax_component (tenant_id, legal_entity_id, code, component, account_key, recoverable_pct, account_id, former_account_ids,
  created_at, created_by, updated_by)
SELECT l.tenant_id, l.legal_entity_id, l.code, 'tax', l.direction, 100, l.account_id,
  coalesce((SELECT array_agg(DISTINCT o.account_id) FROM ledger.tax_code o WHERE o.tenant_id = l.tenant_id AND o.legal_entity_id = l.legal_entity_id
    AND o.code = l.code AND o.account_id <> l.account_id), '{}'), l.created_at, l.created_by, l.created_by
FROM (SELECT DISTINCT ON (tenant_id, legal_entity_id, code) * FROM ledger.tax_code ORDER BY tenant_id, legal_entity_id, code, valid_from DESC) l;
INSERT INTO ledger.tax_rate (tenant_id, id, legal_entity_id, code, component, valid_from, valid_to, rate_pct, created_at, created_by, ended_at, ended_by)
SELECT tenant_id, gen_random_uuid(), legal_entity_id, code, 'tax', valid_from, valid_to, rate_pct, created_at, created_by, ended_at,
  CASE WHEN ended_at IS NOT NULL THEN coalesce(ended_by, created_by) END
FROM ledger.tax_code;
ALTER TABLE ledger.tax_rate ENABLE TRIGGER tax_rate_check;
