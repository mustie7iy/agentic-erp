-- Accounts payable (03-data-model §3.9)

CREATE TABLE payables.supplier_invoice (
  tenant_id                uuid NOT NULL,
  id                       uuid NOT NULL,
  document_number          text NOT NULL,
  legal_entity_id          uuid NOT NULL,
  supplier_id              uuid NOT NULL,       -- partner context
  supplier_reference       text NOT NULL CHECK (length(trim(supplier_reference)) > 0),
  supplier_reference_norm  text GENERATED ALWAYS AS (platform.normalize_ref(supplier_reference)) STORED,
  invoice_date             date NOT NULL,
  posting_date             date,
  currency                 char(3) NOT NULL REFERENCES ref.currency,
  gross_amount             bigint NOT NULL,
  tax_amount               bigint NOT NULL,
  purchase_order_id        uuid,                -- procurement context
  status                   text NOT NULL CHECK (status IN ('parked', 'on_hold', 'posted', 'rejected', 'cancelled', 'reversed')),
  hold_reason              text,
  source                   text NOT NULL CHECK (source IN ('manual', 'capture', 'edi', 'migration')),
  capture_object_ref       text,                -- the original document in object storage
  capture_sha256           bytea CHECK (capture_sha256 IS NULL OR length(capture_sha256) = 32),
  journal_entry_year       smallint,            -- ledger context
  journal_entry_id         uuid,
  reversal_entry_year      smallint,            -- ledger context: the entry that reversed the posting
  reversal_entry_id        uuid,
  workflow_request_id      uuid,                -- workflow context
  extensions               jsonb NOT NULL DEFAULT '{}',
  created_at               timestamptz NOT NULL DEFAULT now(),
  created_by               uuid NOT NULL,
  updated_at               timestamptz NOT NULL DEFAULT now(),
  updated_by               uuid NOT NULL,
  version                  integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, legal_entity_id, document_number),
  CHECK ((status IN ('posted', 'reversed')) = (journal_entry_id IS NOT NULL)),
  CHECK ((journal_entry_id IS NULL) = (journal_entry_year IS NULL)),
  CHECK ((status = 'reversed') = (reversal_entry_id IS NOT NULL)),
  CHECK ((reversal_entry_id IS NULL) = (reversal_entry_year IS NULL)),
  CHECK ((status = 'on_hold') = (hold_reason IS NOT NULL)),
  CHECK ((source = 'capture') = (capture_sha256 IS NOT NULL))
);
-- The same supplier reference cannot be entered twice (duplicate invoice prevention).
-- Rejected, cancelled and reversed invoices free the reference for a corrected entry.
CREATE UNIQUE INDEX supplier_invoice_no_duplicates ON payables.supplier_invoice (tenant_id, supplier_id, supplier_reference_norm)
  WHERE status NOT IN ('rejected', 'cancelled', 'reversed');
-- The same captured document cannot be captured twice.
CREATE UNIQUE INDEX supplier_invoice_capture_once ON payables.supplier_invoice (tenant_id, capture_sha256) WHERE capture_sha256 IS NOT NULL;
CREATE INDEX supplier_invoice_by_status ON payables.supplier_invoice (tenant_id, legal_entity_id, status, invoice_date);
CREATE INDEX supplier_invoice_by_po ON payables.supplier_invoice (tenant_id, purchase_order_id) WHERE purchase_order_id IS NOT NULL;
CREATE TRIGGER supplier_invoice_touch BEFORE UPDATE ON payables.supplier_invoice FOR EACH ROW EXECUTE FUNCTION platform.touch();

CREATE TABLE payables.supplier_invoice_item (
  tenant_id            uuid NOT NULL,
  supplier_invoice_id  uuid NOT NULL,
  item_no              integer NOT NULL CHECK (item_no > 0),
  purchase_order_id    uuid,
  po_item_no           integer,
  description          text,
  quantity             numeric(20,6) NOT NULL CHECK (quantity > 0),
  uom_code             text NOT NULL REFERENCES ref.unit_of_measure,
  amount               bigint NOT NULL,
  tax_code             text,
  account_id           uuid,                      -- required when not PO-based
  PRIMARY KEY (tenant_id, supplier_invoice_id, item_no),
  FOREIGN KEY (tenant_id, supplier_invoice_id) REFERENCES payables.supplier_invoice (tenant_id, id),
  CHECK ((purchase_order_id IS NULL) = (po_item_no IS NULL)),
  CHECK (purchase_order_id IS NOT NULL OR account_id IS NOT NULL)
);

-- History of every three-way match evaluation, with the configuration version used.
CREATE TABLE payables.match_result (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  supplier_invoice_id  uuid NOT NULL,
  evaluated_at         timestamptz NOT NULL DEFAULT now(),
  is_clean             boolean NOT NULL,
  issues               jsonb NOT NULL DEFAULT '[]',
  config_version       integer NOT NULL,
  evaluated_by         uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, supplier_invoice_id, evaluated_at),
  FOREIGN KEY (tenant_id, supplier_invoice_id) REFERENCES payables.supplier_invoice (tenant_id, id),
  CHECK (is_clean = (jsonb_array_length(issues) = 0))
);
CREATE TRIGGER match_result_immutable BEFORE UPDATE OR DELETE ON payables.match_result FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

SELECT platform.create_event_tables('payables');
