-- Validates the check 000205 widened NOT VALID on cash.snapshot_flow (06
-- §6.4), scanning the table without blocking writes.
ALTER TABLE cash.snapshot_flow VALIDATE CONSTRAINT snapshot_flow_document_kind_check;
