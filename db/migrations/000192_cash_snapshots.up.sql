-- Snapshots of the liquidity forecast (T53f; ADR-0051 §6, §15;
-- 03-data-model §3.18): the forecast of a company as it was at a read,
-- taken once a day by the TakeCashSnapshot workflow after the company's
-- snapshot_time, or by a person on demand, so that what was forecast can
-- later be compared with what the bank statements show (the variance,
-- ADR-0051 §11). A snapshot is written whole in one transaction and never
-- changed: insert-only (no version, so platform.secure_table grants no
-- UPDATE), deleted only by the retention of the workflow that takes them
-- (25 months, cash.delete_expired_snapshots). Its flows are at document level for value dates within 31
-- days of the snapshot date (bucket_end null: one row per document a flow
-- pays and liquidity item it is classified under) and summed per bucket of
-- the company's buckets beyond (bucket_end set). legal_entity_id is the
-- tenancy context's, house_bank_id and payment_id the payments context's,
-- supplier_id the partner context's and document_id the owning context's:
-- no foreign key crosses a schema. Expand only: new tables and a
-- permission, which the previous version neither reads nor writes.
CREATE TABLE cash.snapshot (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,     -- tenancy context
  kind             text NOT NULL CHECK (kind IN ('daily', 'on_demand')),
  name             text CHECK (char_length(name) BETWEEN 1 AND 200 AND name !~ '^\s|\s$'),
  snapshot_date    date NOT NULL,     -- the company's date at the read, in time_zone
  time_zone        text NOT NULL CHECK (char_length(time_zone) BETWEEN 1 AND 64),
  horizon_end      date NOT NULL,     -- the last day of the company's last bucket
  config_version   integer NOT NULL CHECK (config_version >= 1),  -- the configuration whose rules classified it
  taken_at         timestamptz NOT NULL DEFAULT now(),
  taken_by         uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  -- A person names the snapshot they take; the daily one is named by its date.
  CONSTRAINT snapshot_name CHECK ((kind = 'on_demand') = (name IS NOT NULL)),
  CONSTRAINT snapshot_horizon CHECK (horizon_end >= snapshot_date)
);
-- One daily snapshot per company and date, however often the workflow runs.
CREATE UNIQUE INDEX snapshot_daily ON cash.snapshot (tenant_id, legal_entity_id, snapshot_date) WHERE kind = 'daily';
-- The list reads a company's snapshots by date; retention deletes by date.
CREATE INDEX snapshot_by_company ON cash.snapshot (tenant_id, legal_entity_id, snapshot_date, id);
SELECT platform.secure_table('cash.snapshot'::regclass);

CREATE TABLE cash.snapshot_flow (
  tenant_id        uuid NOT NULL,
  snapshot_id      uuid NOT NULL,
  line_no          integer NOT NULL CHECK (line_no >= 1),
  flow_no          integer NOT NULL CHECK (flow_no >= 1),  -- the forecast's flow the line is of; a bucket's sum its own
  value_date       date NOT NULL,     -- a flow's day at the bank; a bucket's sum: its first day
  bucket_end       date,              -- null: a flow at document level; else the last day the sum covers
  house_bank_id    uuid,              -- payments context; null: no bank
  currency         char(3) NOT NULL REFERENCES ref.currency,
  source           text NOT NULL CHECK (source ~ '^[a-z][a-z_]{0,39}$'),
  certainty        text NOT NULL CHECK (certainty IN ('actual', 'confirmed', 'expected', 'planned')),
  late             boolean NOT NULL DEFAULT false,
  liquidity_item   text,              -- null: no rule assigns it and no default covers it
  amount           bigint NOT NULL,   -- minor units, signed: a receipt positive
  flow_count       integer NOT NULL CHECK (flow_count >= 1),
  supplier_id      uuid,              -- partner context: the supplier the flow pays
  payment_id       uuid,              -- payments context: a run's payment, or a payment in transit
  document_kind    text CHECK (document_kind IN ('journal_entry', 'supplier_invoice', 'down_payment_request', 'payment_run', 'memo_record')),
  document_id      uuid,
  document_number  text CHECK (char_length(document_number) BETWEEN 1 AND 100),
  PRIMARY KEY (tenant_id, snapshot_id, line_no),
  FOREIGN KEY (tenant_id, snapshot_id) REFERENCES cash.snapshot (tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, liquidity_item) REFERENCES cash.liquidity_item (tenant_id, code),
  CONSTRAINT snapshot_flow_document CHECK ((document_kind IS NULL) = (document_id IS NULL) AND (document_number IS NULL OR document_id IS NOT NULL)),
  -- A bucket's sum names no flow's documents, supplier or payment, and is
  -- never late (a late flow is due the working day after the snapshot).
  CONSTRAINT snapshot_flow_bucket CHECK (bucket_end IS NULL
    OR (bucket_end >= value_date AND document_id IS NULL AND supplier_id IS NULL AND payment_id IS NULL AND NOT late)),
  -- A line at document level is of one flow.
  CONSTRAINT snapshot_flow_count CHECK (bucket_end IS NOT NULL OR flow_count = 1)
);
SELECT platform.secure_table('cash.snapshot_flow'::regclass);
-- Retention (ADR-0051 §6): the application may delete nothing
-- (TestDesignRules); this function, run as the tables' owner and still
-- under row-level security (000011), deletes a company's snapshots dated
-- more than 25 months before the day given, their lines with them, and
-- refuses a day after the database's tomorrow (no zone is further ahead
-- of UTC), so no caller can reach a snapshot younger than its retention.
CREATE FUNCTION cash.delete_expired_snapshots(entity uuid, on_day date)
RETURNS TABLE (deleted_id uuid, deleted_kind text, deleted_date date)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  IF on_day > current_date + 1 THEN
    RAISE EXCEPTION 'snapshot retention on %: a day after tomorrow', on_day
      USING ERRCODE = 'check_violation', CONSTRAINT = 'snapshot_retention_day';
  END IF;
  RETURN QUERY DELETE FROM cash.snapshot s
    WHERE s.legal_entity_id = entity AND s.snapshot_date < (on_day - interval '25 months')::date
    RETURNING s.id, s.kind, s.snapshot_date;
END $$;
ALTER FUNCTION cash.delete_expired_snapshots(uuid, date) OWNER TO erp_owner;
REVOKE EXECUTE ON FUNCTION cash.delete_expired_snapshots(uuid, date) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION cash.delete_expired_snapshots(uuid, date) TO erp_app;

-- The snapshot workflow's dispatcher finds, as the worker role across
-- tenants, the tenants that manage cash: those with liquidity items.
GRANT USAGE ON SCHEMA cash TO erp_worker;
GRANT SELECT (tenant_id, is_active) ON cash.liquidity_item TO erp_worker;

-- Taking a snapshot on demand (ADR-0051 §6, §15); reading one is the
-- forecast's own permission, cash.forecast.read.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('cash.snapshot.take', 'Take a snapshot of the liquidity forecast of a company', false);
