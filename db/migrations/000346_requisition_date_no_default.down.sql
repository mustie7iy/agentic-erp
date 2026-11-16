ALTER TABLE procurement.requisition ALTER COLUMN requisition_date SET DEFAULT (now() AT TIME ZONE 'UTC')::date;
