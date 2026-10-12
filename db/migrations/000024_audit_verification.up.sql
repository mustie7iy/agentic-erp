-- Scheduled audit verification (T21, ADR-0011): a worker recomputes every
-- tenant's chain and checks its anchors in rounds, and records each round
-- here. The latest row per tenant is the alarm state the gauge
-- erp_audit_chains_tampered counts; the history shows when a chain stopped
-- verifying.
CREATE TABLE audit.verification (
  tenant_id        uuid NOT NULL,
  verified_at      timestamptz NOT NULL,
  head_seq         bigint NOT NULL,
  checked          bigint NOT NULL,
  anchors_checked  integer NOT NULL,
  broken_at        bigint,
  ok               boolean NOT NULL,
  PRIMARY KEY (tenant_id, verified_at),
  CHECK (ok = (broken_at IS NULL))
);
CREATE TRIGGER verification_insert_only BEFORE UPDATE OR DELETE ON audit.verification
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('audit.verification'::regclass);
-- The worker finds the tenants with a chain and the gauge counts the chains
-- whose latest verification failed, across tenants.
GRANT SELECT (tenant_id, verified_at, ok) ON audit.verification TO erp_worker;
