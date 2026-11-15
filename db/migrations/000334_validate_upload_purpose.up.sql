-- Validates the uploads' purpose check 000333 added NOT VALID (06 §6.4).
ALTER TABLE payables.upload VALIDATE CONSTRAINT upload_purpose_check;
