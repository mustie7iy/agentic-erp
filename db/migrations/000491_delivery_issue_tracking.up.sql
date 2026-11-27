-- Batches and serial numbers in an outbound delivery's goods issue (S2f2,
-- S2g2; ADR-0083 As built S2f2; 03-data-model §3.36). The goods issue of
-- an item of a batch-managed or serial-managed material names what each
-- line of its 601 issued: its quantity in the item's unit, the batch (by
-- id and number, as the reader needs it beside the id) and the serial
-- numbers of its units, sorted; an item issued from several batches has a
-- line for each. Written with the goods issue and never changed: its
-- reversal (602) gives the same back, which the material document's
-- lines show. batch_id is the inventory context's batch, by id: no
-- foreign key leaves a schema but ref's (02 §2.1). Expand only (06 §6.4):
-- a new table the previous version neither reads nor writes.
CREATE TABLE shipping.outbound_delivery_issue_line (
  tenant_id             uuid NOT NULL,
  outbound_delivery_id  uuid NOT NULL,
  -- The 601's line: one per item it issues (inventory's movement_line_no_check bounds it).
  line_no               smallint NOT NULL CONSTRAINT outbound_delivery_issue_line_no_check CHECK (line_no BETWEEN 1 AND 200),
  item_no               integer NOT NULL,
  quantity              numeric(20,6) NOT NULL CONSTRAINT outbound_delivery_issue_line_quantity_check CHECK (quantity > 0),
  batch_id              uuid,    -- inventory context: inventory.batch
  batch_number          text,    -- inventory.batch.number
  serial_numbers        text[],
  PRIMARY KEY (tenant_id, outbound_delivery_id, line_no),
  CONSTRAINT outbound_delivery_issue_line_item_fkey FOREIGN KEY (tenant_id, outbound_delivery_id, item_no)
    REFERENCES shipping.outbound_delivery_item (tenant_id, outbound_delivery_id, item_no),
  CONSTRAINT outbound_delivery_issue_line_batch CHECK ((batch_id IS NULL) = (batch_number IS NULL)),
  -- A line is written for a batch-managed or serial-managed material's item alone.
  CONSTRAINT outbound_delivery_issue_line_tracked CHECK (batch_id IS NOT NULL OR serial_numbers IS NOT NULL),
  CONSTRAINT outbound_delivery_issue_line_serial_numbers CHECK (cardinality(serial_numbers) BETWEEN 1 AND 1000)
);
CREATE TRIGGER outbound_delivery_issue_line_insert_only BEFORE UPDATE OR DELETE ON shipping.outbound_delivery_issue_line
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('shipping.outbound_delivery_issue_line'::regclass);
