REVOKE UPDATE (purchase_order_id, version, updated_by) ON procurement.schedule_line FROM erp_app;
DROP TRIGGER schedule_line_call_off ON procurement.schedule_line;
DROP FUNCTION procurement.check_schedule_line_call_off();
-- A called-off line reads as entered again: the previous version knows no
-- call-off. The orders created stay, as orders against the agreement.
ALTER TABLE procurement.schedule_line
  DROP CONSTRAINT schedule_line_order_once,
  DROP CONSTRAINT schedule_line_order_fkey,
  DROP COLUMN purchase_order_id;
