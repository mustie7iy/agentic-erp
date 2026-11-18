-- Resetting a clearing (T75i; ADR-0063 §6 As built; 03-data-model §3.7,
-- §3.26): an incoming payment applied to the wrong items, or an
-- application of its credit on account, is undone as SAP's FBRA resets
-- cleared items and reverses the clearing document. The clearing's lines
-- open again: their ledger.clearing_item rows are deleted, which only a
-- reset clearing allows, each kept in ledger.clearing_reset_item by the
-- trigger that lets the delete through; ledger.clearing_reset records the
-- reset of a clearing, once. The clearing entry is then reversed by the
-- ledger's ordinary reversal (ledger.reversal links the two), so the
-- reset holds no reference to it. A line is still cleared at most once at
-- a time (clearing_item_pkey); over time it may be cleared, reset and
-- cleared again.
--
-- receivables.incoming_payment_reset is a reset of a payment's own
-- application (application_no NULL) or of an application of its credit on
-- account, once each, with its reason and the reversal of its clearing
-- entry. A receipt, an open item and a credit line may then be applied
-- again, so the uniques that held them to one application for ever give
-- way to indexes: while one is applied it is cleared, and the ledger's
-- clearing_item_pkey holds it to one application at a time.
--
-- Expand only (06 §6.4): new tables, a trigger that allows what was
-- refused only for a clearing reset, which the previous version never
-- records, and dropped uniques no code of the previous version relies on
-- (no problem is mapped from them; the ledger refuses a second application
-- first).
CREATE TABLE ledger.clearing_reset (
  tenant_id    uuid NOT NULL,
  clearing_id  uuid NOT NULL,
  reset_at     timestamptz NOT NULL DEFAULT now(),
  reset_by     uuid NOT NULL,
  PRIMARY KEY (tenant_id, clearing_id),          -- a clearing is reset once
  FOREIGN KEY (tenant_id, clearing_id) REFERENCES ledger.clearing (tenant_id, id)
);
CREATE TRIGGER clearing_reset_immutable BEFORE UPDATE OR DELETE ON ledger.clearing_reset FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('ledger.clearing_reset'::regclass);

CREATE TABLE ledger.clearing_reset_item (
  tenant_id         uuid NOT NULL,
  clearing_id       uuid NOT NULL,
  fiscal_year       smallint NOT NULL,
  journal_entry_id  uuid NOT NULL,
  line_no           integer NOT NULL,
  PRIMARY KEY (tenant_id, clearing_id, fiscal_year, journal_entry_id, line_no),
  FOREIGN KEY (tenant_id, clearing_id) REFERENCES ledger.clearing_reset (tenant_id, clearing_id),
  FOREIGN KEY (tenant_id, fiscal_year, journal_entry_id, line_no) REFERENCES ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no)
);
CREATE INDEX clearing_reset_item_by_line ON ledger.clearing_reset_item (tenant_id, fiscal_year, journal_entry_id, line_no);
CREATE TRIGGER clearing_reset_item_immutable BEFORE UPDATE OR DELETE ON ledger.clearing_reset_item FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('ledger.clearing_reset_item'::regclass);

-- A clearing item is never changed, and deleted only when its clearing is
-- reset, which keeps it in clearing_reset_item, or when its tenant is
-- erased (platform.forbid_mutation's app.erasing). It runs as the caller,
-- held to row-level security like the delete itself.
CREATE FUNCTION ledger.clearing_item_reset_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF coalesce(current_setting('app.erasing', true), '') <> '' AND current_setting('app.erasing', true) = OLD.tenant_id::text THEN
      RETURN OLD;
    END IF;
    IF EXISTS (SELECT FROM ledger.clearing_reset r WHERE r.tenant_id = OLD.tenant_id AND r.clearing_id = OLD.clearing_id) THEN
      INSERT INTO ledger.clearing_reset_item (tenant_id, clearing_id, fiscal_year, journal_entry_id, line_no)
      VALUES (OLD.tenant_id, OLD.clearing_id, OLD.fiscal_year, OLD.journal_entry_id, OLD.line_no);
      RETURN OLD;
    END IF;
  END IF;
  RAISE EXCEPTION 'ledger.clearing_item is insert-only; % is allowed only of a clearing that is reset', TG_OP
    USING ERRCODE = 'insufficient_privilege';
END $$;
ALTER FUNCTION ledger.clearing_item_reset_only() OWNER TO erp_owner;
DROP TRIGGER clearing_item_immutable ON ledger.clearing_item;
CREATE TRIGGER clearing_item_reset_only BEFORE UPDATE OR DELETE ON ledger.clearing_item FOR EACH ROW EXECUTE FUNCTION ledger.clearing_item_reset_only();
GRANT DELETE ON ledger.clearing_item TO erp_app;

CREATE TABLE receivables.incoming_payment_reset (
  tenant_id             uuid NOT NULL,
  incoming_payment_id   uuid NOT NULL,
  application_no        integer,             -- NULL: the payment's own application of its receipt
  reason                text NOT NULL CONSTRAINT incoming_payment_reset_reason_length CHECK (length(reason) BETWEEN 1 AND 200),
  reversal_entry_year   smallint NOT NULL,
  reversal_entry_id     uuid NOT NULL,       -- ledger context: the reversal of the clearing entry
  created_at            timestamptz NOT NULL DEFAULT now(),
  created_by            uuid NOT NULL,
  CONSTRAINT incoming_payment_reset_payment_fkey FOREIGN KEY (tenant_id, incoming_payment_id) REFERENCES receivables.incoming_payment (tenant_id, id),
  CONSTRAINT incoming_payment_reset_application_fkey FOREIGN KEY (tenant_id, incoming_payment_id, application_no)
    REFERENCES receivables.incoming_payment_application (tenant_id, incoming_payment_id, application_no),
  -- Each application, the payment's own included, is reset once, and a
  -- reversal is one reset's.
  CONSTRAINT incoming_payment_reset_once UNIQUE NULLS NOT DISTINCT (tenant_id, incoming_payment_id, application_no),
  CONSTRAINT incoming_payment_reset_entry_once UNIQUE (tenant_id, reversal_entry_id)
);
CREATE TRIGGER incoming_payment_reset_immutable BEFORE UPDATE OR DELETE ON receivables.incoming_payment_reset
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('receivables.incoming_payment_reset'::regclass);

ALTER TABLE receivables.incoming_payment DROP CONSTRAINT incoming_payment_receipt_once;
CREATE INDEX incoming_payment_by_receipt ON receivables.incoming_payment (tenant_id, receipt_entry_id);
ALTER TABLE receivables.incoming_payment_item DROP CONSTRAINT incoming_payment_item_once;
CREATE INDEX incoming_payment_item_by_line ON receivables.incoming_payment_item (tenant_id, journal_entry_id, line_no);
ALTER TABLE receivables.incoming_payment_application DROP CONSTRAINT incoming_payment_application_credit_once;
CREATE INDEX incoming_payment_application_by_credit ON receivables.incoming_payment_application (tenant_id, credit_entry_id, credit_line_no);
