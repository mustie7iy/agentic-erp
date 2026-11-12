DROP TABLE procurement.schedule_line;
DROP FUNCTION procurement.check_schedule_line();
DROP TRIGGER contract_kind_kept ON procurement.contract;
DROP FUNCTION procurement.check_contract_kind();
-- A scheduling agreement reads as a contract again: the previous version
-- knows no other kind, and its schedule went with the table.
ALTER TABLE procurement.contract DROP COLUMN kind;
