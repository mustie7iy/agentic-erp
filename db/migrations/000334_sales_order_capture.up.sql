-- Order capture (T90a; ADR-0067 As built T90a; ADR-0020; 03-data-model
-- §3.29): a sales representative uploads a customer's purchase order
-- document and capture reads it into a draft sales order. The order keeps
-- the upload it was captured from, once, and what capture read: the
-- customer's number and tax registration number as printed and per item
-- what was read and where its price came from. Uploads take a second
-- purpose, sales_order. Expand only (06 §6.4): two nullable columns, a
-- partial unique index, an index, a wider check and one permission, which the
-- previous version neither reads nor writes.

-- capture_upload_id is the payables context's upload the order was
-- captured from (no foreign key leaves the schema, 02 §2.1); capture is
-- what was read, a JSON object, present exactly with the upload. An order
-- created by hand has neither.
ALTER TABLE sales.sales_order
  ADD COLUMN capture_upload_id uuid,   -- payables context: the upload
  ADD COLUMN capture jsonb,
  ADD CONSTRAINT sales_order_capture_check CHECK ((capture_upload_id IS NULL) = (capture IS NULL)
    AND (capture IS NULL OR jsonb_typeof(capture) = 'object'));
-- One upload gives one order: the same upload captured again answers it.
CREATE UNIQUE INDEX sales_order_one_per_upload ON sales.sales_order (tenant_id, capture_upload_id) WHERE capture_upload_id IS NOT NULL;

-- A direct upload may carry a customer's purchase order for order
-- capture. NOT VALID reads no row under the lock; every row satisfied the
-- narrower check, and the next migration validates the new one.
ALTER TABLE payables.upload DROP CONSTRAINT upload_purpose_check;
ALTER TABLE payables.upload ADD CONSTRAINT upload_purpose_check CHECK (purpose IN ('supplier_invoice', 'sales_order')) NOT VALID;

-- Order capture finds a customer by a tax registration number of any
-- scheme, by its keyed hash alone.
CREATE INDEX tax_identifier_by_fingerprint ON partner.tax_identifier (tenant_id, value_fingerprint);

-- Capturing sales orders (ADR-0067).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('sales.order.capture', 'Capture sales orders from a customer''s purchase order document, as drafts', false);
