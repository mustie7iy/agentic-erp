-- Payment terms frozen at posting (T36b, 03-data-model §3.7 and §3.9), as
-- SAP keeps ZTERM, ZFBDT and ZBD1T..ZBD3T in the invoice header (RBKP) and
-- copies them into the vendor line (BSEG): a supplier invoice keeps the
-- terms it was posted with, and so does its open item, so a later change
-- to a payment term changes neither its net due date nor the cash
-- discount it earns. Baseline date, net days and up to two discount
-- tiers, with the rules of partner.payment_term (000004, 000067). Written
-- in the posting transaction and never changed: like the line, history.
-- Separate tables, so neither the partitioned journal nor the invoices
-- are altered; an invoice or item posted before T36 has no row.
CREATE TABLE ledger.open_item_terms (
  tenant_id          uuid NOT NULL,
  fiscal_year        smallint NOT NULL,
  journal_entry_id   uuid NOT NULL,
  line_no            integer NOT NULL,
  payment_term_code  text NOT NULL,              -- partner context: the term at posting
  baseline_date      date NOT NULL,
  net_days           smallint NOT NULL CHECK (net_days BETWEEN 0 AND 365),
  discount_days      smallint CHECK (discount_days BETWEEN 0 AND 365),
  discount_pct       numeric(9,6) CHECK (discount_pct > 0 AND discount_pct < 100),
  discount2_days     smallint CHECK (discount2_days BETWEEN 0 AND 365),
  discount2_pct      numeric(9,6) CHECK (discount2_pct > 0 AND discount2_pct < 100),
  created_at         timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, fiscal_year, journal_entry_id, line_no),
  CONSTRAINT open_item_terms_line_fkey FOREIGN KEY (tenant_id, fiscal_year, journal_entry_id, line_no) REFERENCES ledger.journal_line (tenant_id, fiscal_year, journal_entry_id, line_no),
  CONSTRAINT open_item_terms_pairs CHECK ((discount_days IS NULL) = (discount_pct IS NULL) AND (discount2_days IS NULL) = (discount2_pct IS NULL)),
  CONSTRAINT open_item_terms_tiers CHECK ((discount_days IS NULL OR discount_days < net_days)
    AND (discount2_days IS NULL OR (discount_days IS NOT NULL AND discount_days < discount2_days AND discount2_days < net_days
      AND discount2_pct < discount_pct)))
);
CREATE TRIGGER open_item_terms_immutable BEFORE UPDATE OR DELETE ON ledger.open_item_terms FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('ledger.open_item_terms'::regclass);

CREATE TABLE payables.supplier_invoice_terms (
  tenant_id            uuid NOT NULL,
  supplier_invoice_id  uuid NOT NULL,
  payment_term_code    text NOT NULL,            -- partner context: the term at posting
  baseline_date        date NOT NULL,
  net_days             smallint NOT NULL CHECK (net_days BETWEEN 0 AND 365),
  discount_days        smallint CHECK (discount_days BETWEEN 0 AND 365),
  discount_pct         numeric(9,6) CHECK (discount_pct > 0 AND discount_pct < 100),
  discount2_days       smallint CHECK (discount2_days BETWEEN 0 AND 365),
  discount2_pct        numeric(9,6) CHECK (discount2_pct > 0 AND discount2_pct < 100),
  created_at           timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, supplier_invoice_id),
  FOREIGN KEY (tenant_id, supplier_invoice_id) REFERENCES payables.supplier_invoice (tenant_id, id),
  CONSTRAINT supplier_invoice_terms_pairs CHECK ((discount_days IS NULL) = (discount_pct IS NULL) AND (discount2_days IS NULL) = (discount2_pct IS NULL)),
  CONSTRAINT supplier_invoice_terms_tiers CHECK ((discount_days IS NULL OR discount_days < net_days)
    AND (discount2_days IS NULL OR (discount_days IS NOT NULL AND discount_days < discount2_days AND discount2_days < net_days
      AND discount2_pct < discount_pct)))
);
CREATE TRIGGER supplier_invoice_terms_immutable BEFORE UPDATE OR DELETE ON payables.supplier_invoice_terms
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('payables.supplier_invoice_terms'::regclass);
