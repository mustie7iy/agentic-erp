-- Asset acquisitions from posted lines on an asset clearing account (T57c;
-- ADR-0057 §5, §6; 03-data-model §3.21): the acquisitions, what is used of
-- each capitalised line, the assets' movements (the subledger) and the
-- journal entries of each assets document per ledger. legal_entity_id is
-- the tenancy context's, ledger_id, the journal entries and lines the
-- ledger's: no foreign key leaves the schema. Expand only: new tables the
-- previous version neither reads nor writes.

-- An acquisition (§6): an asset's cost from a journal line on its class's
-- clearing account (source_*), or by manual entry against an offset
-- account (T57d), in the line's currency with its local and group amounts.
-- seq is the asset's posting order (§5). Posted, then reversed (T57d).
CREATE TABLE assets.acquisition (
  tenant_id          uuid NOT NULL,
  id                 uuid NOT NULL,
  legal_entity_id    uuid NOT NULL,      -- tenancy context
  asset_id           uuid NOT NULL,
  seq                integer NOT NULL CHECK (seq >= 1),
  value_date         date NOT NULL,
  posting_date       date NOT NULL,
  fiscal_year        smallint NOT NULL,  -- the posting date's
  currency           char(3) NOT NULL REFERENCES ref.currency,
  amount             bigint NOT NULL CHECK (amount <> 0),   -- negative from a credit line
  amount_lc          bigint NOT NULL,
  amount_gc          bigint NOT NULL,
  currency_lc        char(3) NOT NULL REFERENCES ref.currency,
  currency_gc        char(3) NOT NULL REFERENCES ref.currency,
  source_entry_year  smallint,           -- ledger context: the capitalised line, with its entry's
  source_entry_id    uuid,               -- document number and origin as they were posted
  source_line_no     integer,
  source_document_number text,
  source_context     text,
  source_document_id uuid,               -- the source context's document (a supplier invoice)
  offset_account_id  uuid,               -- ledger context (T57d)
  text               text CHECK (length(text) <= 200),
  status             text NOT NULL CHECK (status IN ('posted', 'reversed')),
  version            integer NOT NULL DEFAULT 1,
  created_at         timestamptz NOT NULL DEFAULT now(),
  created_by         uuid NOT NULL,
  updated_at         timestamptz NOT NULL DEFAULT now(),
  updated_by         uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, asset_id) REFERENCES assets.asset (tenant_id, id),
  CONSTRAINT acquisition_seq_once UNIQUE (tenant_id, asset_id, seq),
  -- From a line or against an offset account, never both.
  CONSTRAINT acquisition_source CHECK (
    (source_entry_id IS NULL) = (offset_account_id IS NOT NULL)
    AND (source_entry_id IS NULL) = (source_entry_year IS NULL) AND (source_entry_id IS NULL) = (source_line_no IS NULL)
    AND (source_entry_id IS NULL) = (source_document_number IS NULL) AND (source_entry_id IS NULL) = (source_context IS NULL))
);
CREATE TRIGGER acquisition_touch BEFORE UPDATE ON assets.acquisition FOR EACH ROW EXECUTE FUNCTION platform.touch();
CREATE INDEX acquisition_by_company ON assets.acquisition (tenant_id, legal_entity_id, posting_date, id);
CREATE INDEX acquisition_by_asset ON assets.acquisition (tenant_id, asset_id, seq);
CREATE INDEX acquisition_by_source ON assets.acquisition (tenant_id, source_entry_id, source_line_no) WHERE source_entry_id IS NOT NULL;
SELECT platform.secure_table('assets.acquisition'::regclass);

-- An acquisition starts posted and is reversed at most once; what it
-- posted never changes.
CREATE FUNCTION assets.check_acquisition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'posted' THEN
      RAISE EXCEPTION 'an acquisition starts posted, not %', NEW.status USING ERRCODE = 'check_violation', CONSTRAINT = 'acquisition_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF NEW.status <> OLD.status AND (OLD.status, NEW.status) <> ('posted', 'reversed') THEN
    RAISE EXCEPTION 'acquisition % cannot change from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'acquisition_transition';
  END IF;
  IF (NEW.legal_entity_id, NEW.asset_id, NEW.seq, NEW.value_date, NEW.posting_date, NEW.fiscal_year, NEW.currency, NEW.amount, NEW.amount_lc,
      NEW.amount_gc, NEW.currency_lc, NEW.currency_gc, NEW.source_entry_year, NEW.source_entry_id, NEW.source_line_no, NEW.source_document_number,
      NEW.source_context, NEW.source_document_id, NEW.offset_account_id, NEW.text, NEW.created_at, NEW.created_by)
      IS DISTINCT FROM (OLD.legal_entity_id, OLD.asset_id, OLD.seq, OLD.value_date, OLD.posting_date, OLD.fiscal_year, OLD.currency, OLD.amount,
      OLD.amount_lc, OLD.amount_gc, OLD.currency_lc, OLD.currency_gc, OLD.source_entry_year, OLD.source_entry_id, OLD.source_line_no,
      OLD.source_document_number, OLD.source_context, OLD.source_document_id, OLD.offset_account_id, OLD.text, OLD.created_at, OLD.created_by) THEN
    RAISE EXCEPTION 'acquisition % keeps what it posted', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'acquisition_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION assets.check_acquisition() OWNER TO erp_owner;
CREATE TRIGGER acquisition_transition BEFORE INSERT OR UPDATE ON assets.acquisition FOR EACH ROW EXECUTE FUNCTION assets.check_acquisition();

-- What is used of a capitalised line (§5, §6): the line's amounts in its
-- currency, local and group currency, and what acquisitions took of each,
-- which stays between zero and the line's amount (a credit line's
-- negative). Read FOR UPDATE by every acquisition from the line, after an
-- insert that does nothing when the row is there, so two acquisitions from
-- one line serialise.
CREATE TABLE assets.clearing_use (
  tenant_id        uuid NOT NULL,
  entry_year       smallint NOT NULL,  -- ledger context: the line
  entry_id         uuid NOT NULL,
  line_no          integer NOT NULL,
  legal_entity_id  uuid NOT NULL,      -- tenancy context
  currency         char(3) NOT NULL REFERENCES ref.currency,
  line_amount      bigint NOT NULL CHECK (line_amount <> 0),
  line_lc          bigint NOT NULL,
  line_gc          bigint NOT NULL,
  used             bigint NOT NULL DEFAULT 0,
  used_lc          bigint NOT NULL DEFAULT 0,
  used_gc          bigint NOT NULL DEFAULT 0,
  version          integer NOT NULL DEFAULT 1,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, entry_year, entry_id, line_no),
  CONSTRAINT clearing_use_within_line CHECK (used BETWEEN least(0, line_amount) AND greatest(0, line_amount)
    AND used_lc BETWEEN least(0, line_lc) AND greatest(0, line_lc) AND used_gc BETWEEN least(0, line_gc) AND greatest(0, line_gc))
);
CREATE TRIGGER clearing_use_touch BEFORE UPDATE ON assets.clearing_use FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The worklist reads a company's used lines of a year.
CREATE INDEX clearing_use_by_company ON assets.clearing_use (tenant_id, legal_entity_id, entry_year);
SELECT platform.secure_table('assets.clearing_use'::regclass);

-- The line's amounts are the posted line's, which never change.
CREATE FUNCTION assets.check_clearing_use() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.legal_entity_id, NEW.currency, NEW.line_amount, NEW.line_lc, NEW.line_gc)
      IS DISTINCT FROM (OLD.legal_entity_id, OLD.currency, OLD.line_amount, OLD.line_lc, OLD.line_gc) THEN
    RAISE EXCEPTION 'line % of entry % keeps its amounts', OLD.line_no, OLD.entry_id USING ERRCODE = 'check_violation', CONSTRAINT = 'clearing_use_line_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION assets.check_clearing_use() OWNER TO erp_owner;
CREATE TRIGGER clearing_use_line_fixed BEFORE UPDATE ON assets.clearing_use FOR EACH ROW EXECUTE FUNCTION assets.check_clearing_use();

-- The assets' movements (§5), the subledger: one row per asset, ledger and
-- effect, cost and accumulated the signed changes of acquisition cost and
-- accumulated depreciation in local currency, *_gc the same in group
-- currency as the ledger booked them, residual and proceeds what a
-- retirement or transfer took or brought. Append-only: a reversal is a row
-- negating its original (reverses_id), and each original is reversed at
-- most once.
CREATE TABLE assets.movement (
  tenant_id                    uuid NOT NULL,
  id                           uuid NOT NULL,
  asset_id                     uuid NOT NULL,
  seq                          integer NOT NULL CHECK (seq >= 1),
  legal_entity_id              uuid NOT NULL,      -- tenancy context
  ledger_id                    uuid NOT NULL,      -- ledger context
  class_code                   text NOT NULL,
  kind                         text NOT NULL CHECK (kind IN ('acquisition', 'depreciation', 'retirement', 'transfer_out', 'transfer_in')),
  value_date                   date NOT NULL,
  posting_date                 date NOT NULL,
  fiscal_year                  smallint NOT NULL,
  fiscal_period                smallint NOT NULL CHECK (fiscal_period BETWEEN 1 AND 16),
  cost                         bigint NOT NULL DEFAULT 0,
  accumulated                  bigint NOT NULL DEFAULT 0,
  cost_gc                      bigint NOT NULL DEFAULT 0,
  accumulated_gc               bigint NOT NULL DEFAULT 0,
  residual                     bigint NOT NULL DEFAULT 0,
  proceeds                     bigint NOT NULL DEFAULT 0,
  share                        numeric,
  document_kind                text NOT NULL CHECK (document_kind IN ('acquisition', 'retirement', 'transfer', 'depreciation_run')),
  document_id                  uuid NOT NULL,
  journal_entry_year           smallint,           -- ledger context; none for a catch-up of zero (§5)
  journal_entry_id             uuid,
  reverses_id                  uuid,
  booked_through_before_year   smallint,
  booked_through_before_period smallint,
  created_at                   timestamptz NOT NULL DEFAULT now(),
  created_by                   uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, asset_id) REFERENCES assets.asset (tenant_id, id),
  FOREIGN KEY (tenant_id, reverses_id) REFERENCES assets.movement (tenant_id, id),
  -- An original is reversed at most once.
  CONSTRAINT movement_reversed_once UNIQUE (tenant_id, reverses_id),
  -- Depreciation changes accumulated depreciation alone.
  CONSTRAINT movement_depreciation CHECK (kind <> 'depreciation' OR (cost = 0 AND cost_gc = 0 AND residual = 0 AND proceeds = 0)),
  CONSTRAINT movement_entry CHECK ((journal_entry_year IS NULL) = (journal_entry_id IS NULL)),
  CONSTRAINT movement_booked_through CHECK ((booked_through_before_year IS NULL) = (booked_through_before_period IS NULL))
);
CREATE TRIGGER movement_append_only BEFORE UPDATE OR DELETE ON assets.movement FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE INDEX movement_by_asset ON assets.movement (tenant_id, asset_id, ledger_id, seq);
CREATE INDEX movement_by_document ON assets.movement (tenant_id, document_kind, document_id);
SELECT platform.secure_table('assets.movement'::regclass);

-- A reversal row is its original's kind, of the same asset and ledger,
-- with exactly the negated amounts, and reverses no reversal (§5); the
-- sign rule binds reversals only, since an acquisition from a credit line
-- is negative and depreciation may have either sign.
CREATE FUNCTION assets.check_movement() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE o assets.movement;
BEGIN
  IF NEW.reverses_id IS NULL THEN
    RETURN NEW;
  END IF;
  SELECT * INTO o FROM assets.movement m WHERE m.tenant_id = NEW.tenant_id AND m.id = NEW.reverses_id;
  IF o.reverses_id IS NOT NULL OR (NEW.kind, NEW.asset_id, NEW.ledger_id) IS DISTINCT FROM (o.kind, o.asset_id, o.ledger_id)
      OR (NEW.cost, NEW.accumulated, NEW.cost_gc, NEW.accumulated_gc, NEW.residual, NEW.proceeds)
      IS DISTINCT FROM (-o.cost, -o.accumulated, -o.cost_gc, -o.accumulated_gc, -o.residual, -o.proceeds) THEN
    RAISE EXCEPTION 'movement % does not negate movement %: a reversal has its original''s kind, asset and ledger and its amounts negated',
      NEW.id, NEW.reverses_id USING ERRCODE = 'check_violation', CONSTRAINT = 'movement_reversal';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION assets.check_movement() OWNER TO erp_owner;
CREATE TRIGGER movement_reversal BEFORE INSERT ON assets.movement FOR EACH ROW EXECUTE FUNCTION assets.check_movement();

-- The journal entry of each assets document per ledger and part (§5):
-- main, or a center transfer's catch_up (T57h). The reversal's entry is
-- recorded by the reversal (T57d), which grants the update it needs.
CREATE TABLE assets.document_entry (
  tenant_id            uuid NOT NULL,
  document_kind        text NOT NULL CHECK (document_kind IN ('acquisition', 'retirement', 'transfer')),
  document_id          uuid NOT NULL,
  ledger_id            uuid NOT NULL,      -- ledger context
  part                 text NOT NULL CHECK (part IN ('main', 'catch_up')),
  journal_entry_year   smallint NOT NULL,  -- ledger context
  journal_entry_id     uuid NOT NULL,
  document_number      text NOT NULL,
  reversal_entry_year  smallint,
  reversal_entry_id    uuid,
  created_at           timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, document_kind, document_id, ledger_id, part),
  CONSTRAINT document_entry_reversal CHECK ((reversal_entry_year IS NULL) = (reversal_entry_id IS NULL))
);
SELECT platform.secure_table('assets.document_entry'::regclass);

-- Posting assets documents (ADR-0057 §10).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('assets.asset.post', 'Post and reverse asset acquisitions, retirements and transfers', false);
