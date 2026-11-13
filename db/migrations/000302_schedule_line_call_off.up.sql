-- A schedule line called off (T83b; ADR-0070 §4, plan-breadth-week2
-- §T83b): calling off a line creates a draft purchase order against its
-- scheduling agreement, and the line names that order. A line is called
-- off once and is then fixed; nothing else of a line ever changes. The
-- order is any purchase order, so receipts, GR/IR and invoices know
-- nothing of schedule lines. Expand only: a nullable column, which the
-- previous version neither reads nor writes (06 §6.4); it shows a called
-- off line as it shows any line.
ALTER TABLE procurement.schedule_line
  ADD COLUMN purchase_order_id uuid,
  ADD CONSTRAINT schedule_line_order_fkey FOREIGN KEY (tenant_id, purchase_order_id)
    REFERENCES procurement.purchase_order (tenant_id, id),
  -- One order calls off one line; the index also finds an order's line.
  ADD CONSTRAINT schedule_line_order_once UNIQUE (tenant_id, purchase_order_id);

-- A line is entered not called off; the one change of a line is its
-- call-off, once, to a purchase order against its own agreement. What
-- the line asks for stays as entered: the application role may set
-- nothing else (the column grant below).
CREATE FUNCTION procurement.check_schedule_line_call_off() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.purchase_order_id IS NOT NULL THEN
      RAISE EXCEPTION 'schedule line % is entered not called off', NEW.id
        USING ERRCODE = 'check_violation', CONSTRAINT = 'schedule_line_starts_scheduled';
    END IF;
    RETURN NEW;
  END IF;
  IF OLD.purchase_order_id IS NOT NULL THEN
    RAISE EXCEPTION 'schedule line % was called off: it is fixed', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'schedule_line_called_off_once';
  END IF;
  IF NOT EXISTS (SELECT FROM procurement.purchase_order o WHERE o.tenant_id = NEW.tenant_id AND o.id = NEW.purchase_order_id
      AND o.contract_id = NEW.contract_id) THEN
    RAISE EXCEPTION 'schedule line % is called off as a purchase order against its own scheduling agreement', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'schedule_line_order_of_agreement';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION procurement.check_schedule_line_call_off() OWNER TO erp_owner;
CREATE TRIGGER schedule_line_call_off BEFORE INSERT OR UPDATE ON procurement.schedule_line
  FOR EACH ROW EXECUTE FUNCTION procurement.check_schedule_line_call_off();
-- What a call-off sets, and nothing else of a line (000301 revoked UPDATE
-- until this slice).
GRANT UPDATE (purchase_order_id, version, updated_by) ON procurement.schedule_line TO erp_app;
