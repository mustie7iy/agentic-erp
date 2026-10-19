-- EBICS refused in production (T44d; ADR-0038; 03-data-model §3.10): the
-- server refuses to start while any tenant has an active bank_ebics
-- connector, which it reads as the control role across tenants, as it reads
-- identity providers (000056). It reads what decides the refusal, not the
-- connector's settings or secret reference.
GRANT USAGE ON SCHEMA integration TO erp_control;
GRANT SELECT (tenant_id, id, kind, status) ON integration.connector TO erp_control;
