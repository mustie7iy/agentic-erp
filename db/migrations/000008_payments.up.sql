-- Payments (03-data-model §3.10)

CREATE TABLE payments.house_bank (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,
  code             text NOT NULL CHECK (code ~ '^[A-Z0-9]{1,5}$'),
  name             text NOT NULL,
  iban_cipher      bytea NOT NULL,
  iban_last4       text NOT NULL,
  bic              text NOT NULL CHECK (bic ~ '^[A-Z]{6}[A-Z0-9]{2}([A-Z0-9]{3})?$'),
  currency         char(3) NOT NULL REFERENCES ref.currency,
  gl_account_id    uuid NOT NULL,               -- ledger context
  status           text NOT NULL CHECK (status IN ('active', 'inactive')),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  version          integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, legal_entity_id, code)
);
CREATE TRIGGER house_bank_touch BEFORE UPDATE ON payments.house_bank FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE payments.payment_run (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  document_number      text NOT NULL,
  legal_entity_id      uuid NOT NULL,
  house_bank_id        uuid NOT NULL,
  run_date             date NOT NULL,
  due_by               date NOT NULL,
  currency             char(3) NOT NULL REFERENCES ref.currency,
  total_amount         bigint NOT NULL CHECK (total_amount >= 0),
  status               text NOT NULL CHECK (status IN ('proposing', 'proposed', 'released', 'rejected', 'failed')),
  proposed_by          uuid NOT NULL,
  decided_by           uuid,                    -- who released or rejected the run
  decided_at           timestamptz,
  operation_id         uuid,                    -- platform.operation for large runs
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid NOT NULL,
  version              integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, legal_entity_id, document_number),
  FOREIGN KEY (tenant_id, house_bank_id) REFERENCES payments.house_bank (tenant_id, id),
  CHECK (status NOT IN ('released', 'rejected') OR decided_by IS NOT NULL),
  CHECK ((decided_by IS NULL) = (decided_at IS NULL)),
  -- Segregation of duties: whoever proposed a run can never release or reject it.
  CONSTRAINT payment_run_sod CHECK (decided_by IS NULL OR decided_by <> proposed_by)
);
CREATE TRIGGER payment_run_touch BEFORE UPDATE ON payments.payment_run FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE payments.payment (
  tenant_id           uuid NOT NULL,
  id                  uuid NOT NULL,
  payment_run_id      uuid NOT NULL,
  supplier_id         uuid NOT NULL,            -- partner context
  bank_account_id     uuid NOT NULL,            -- partner context, the account valid on the run date
  amount              bigint NOT NULL CHECK (amount > 0),
  currency            char(3) NOT NULL REFERENCES ref.currency,
  end_to_end_id       text NOT NULL CHECK (length(end_to_end_id) <= 35),
  journal_entry_year  smallint,
  journal_entry_id    uuid,
  created_at          timestamptz NOT NULL DEFAULT now(),
  created_by          uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, payment_run_id, supplier_id),
  UNIQUE (tenant_id, end_to_end_id),
  FOREIGN KEY (tenant_id, payment_run_id) REFERENCES payments.payment_run (tenant_id, id),
  CHECK ((journal_entry_id IS NULL) = (journal_entry_year IS NULL))
);

-- Open items settled by a payment. An item is in at most one live run.
CREATE TABLE payments.payment_item (
  tenant_id           uuid NOT NULL,
  payment_id          uuid NOT NULL,
  item_fiscal_year    smallint NOT NULL,         -- ledger context: the open item
  item_entry_id       uuid NOT NULL,
  item_line_no        integer NOT NULL,
  amount              bigint NOT NULL CHECK (amount > 0),
  due_date            date NOT NULL,
  is_active           boolean NOT NULL DEFAULT true,   -- false once the run is rejected or failed
  PRIMARY KEY (tenant_id, payment_id, item_fiscal_year, item_entry_id, item_line_no),
  FOREIGN KEY (tenant_id, payment_id) REFERENCES payments.payment (tenant_id, id)
);
CREATE UNIQUE INDEX payment_item_reserved_once ON payments.payment_item (tenant_id, item_fiscal_year, item_entry_id, item_line_no) WHERE is_active;
-- Only is_active may change, and only from true to false.
CREATE FUNCTION payments.check_item_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.tenant_id, NEW.payment_id, NEW.item_fiscal_year, NEW.item_entry_id, NEW.item_line_no, NEW.amount, NEW.due_date)
     IS DISTINCT FROM (OLD.tenant_id, OLD.payment_id, OLD.item_fiscal_year, OLD.item_entry_id, OLD.item_line_no, OLD.amount, OLD.due_date)
     OR (NEW.is_active AND NOT OLD.is_active) THEN
    RAISE EXCEPTION 'payment items can only be deactivated' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER payment_item_update BEFORE UPDATE ON payments.payment_item FOR EACH ROW EXECUTE FUNCTION payments.check_item_update();

CREATE TABLE payments.payment_exclusion (
  tenant_id       uuid NOT NULL,
  payment_run_id  uuid NOT NULL,
  supplier_id     uuid NOT NULL,
  reason_code     text NOT NULL CHECK (reason_code IN ('supplier_blocked', 'payment_blocked', 'no_bank_account', 'bank_change_pending', 'bank_change_hold', 'debit_balance')),
  amount          bigint NOT NULL,
  PRIMARY KEY (tenant_id, payment_run_id, supplier_id),
  FOREIGN KEY (tenant_id, payment_run_id) REFERENCES payments.payment_run (tenant_id, id)
);

CREATE TABLE payments.bank_file (
  tenant_id       uuid NOT NULL,
  id              uuid NOT NULL,
  payment_run_id  uuid NOT NULL,
  format          text NOT NULL CHECK (format IN ('pain.001.001.03', 'pain.001.001.09')),
  object_ref      text NOT NULL,                  -- object storage key
  sha256          bytea NOT NULL CHECK (length(sha256) = 32),
  status          text NOT NULL CHECK (status IN ('created', 'transmitted', 'acknowledged', 'rejected')),
  created_at      timestamptz NOT NULL DEFAULT now(),
  created_by      uuid NOT NULL,
  updated_at      timestamptz NOT NULL DEFAULT now(),
  version         integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, payment_run_id) REFERENCES payments.payment_run (tenant_id, id)
);
CREATE TRIGGER bank_file_touch BEFORE UPDATE ON payments.bank_file FOR EACH ROW EXECUTE FUNCTION platform.touch();

SELECT platform.create_event_tables('payments');
