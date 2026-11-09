-- Validates the check 000231 widened NOT VALID on integration.connector
-- (06 §6.4), scanning the table without blocking writes.
ALTER TABLE integration.connector VALIDATE CONSTRAINT connector_kind_check;
