-- Analytical store (ADR-0044, T55b). The expand step of the journal's
-- extract watermark: each entry records the transaction that posted it.
-- Without a default in ADD COLUMN, no partition is rewritten; entries posted
-- before this migration keep NULL, which every extract's first run reads
-- (they committed before any cutoff it can take).
ALTER TABLE ledger.journal_entry ADD COLUMN posted_xid xid8;
ALTER TABLE ledger.journal_entry ALTER COLUMN posted_xid SET DEFAULT pg_current_xact_id();
-- An extract reads one tenant's entries above its previous cutoff.
CREATE INDEX journal_entry_posted_xid ON ledger.journal_entry (tenant_id, posted_xid);

-- The pointer to a tenant's current manifest: one row per tenant, so all of
-- its datasets are swapped together and a reader never sees two runs mixed.
-- Derived data (ADR-0044): rebuilt after a restore or a move, so the
-- watermark, a transaction id of this cluster, never outlives it.
CREATE TABLE analytics.snapshot (
  tenant_id     uuid NOT NULL,
  manifest_key  text NOT NULL CONSTRAINT snapshot_manifest_key_check
                CHECK (manifest_key = 'analytics/v1/tenant=' || tenant_id || '/manifest-' || lpad(watermark::text, 20, '0') || '.json'),
  watermark     xid8 NOT NULL,   -- the cutoff of the run that wrote the manifest: its snapshot's xmin
  as_of         timestamptz NOT NULL,
  published_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id)
);
SELECT platform.secure_table('analytics.snapshot');
-- A run publishes by moving the pointer, guarded by the manifest it read.
GRANT UPDATE (manifest_key, watermark, as_of, published_at) ON analytics.snapshot TO erp_app;
