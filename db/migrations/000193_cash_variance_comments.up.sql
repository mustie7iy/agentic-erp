-- Variance comments (T53f4; ADR-0051 §6, §11, §13, §15; 03-data-model
-- §3.18): a person's explanation of a liquidity item's forecast variance
-- (or of what no rule assigns, liquidity_item null) for a company over a
-- period at a lag, against the snapshot that variance compares with. A
-- person's comment is decided as written (status accepted, decided by its
-- author); the forecast agent's proposals (T53i) are proposed until a
-- person accepts or rejects them, so the status holds all three now.
-- Insert-only today (no version, so platform.secure_table grants no
-- UPDATE), and never deleted. A comment keeps its snapshot: the retention
-- function now skips a snapshot a comment refers to, and the foreign key
-- refuses deleting one any other way. legal_entity_id is the tenancy
-- context's: no foreign key crosses a schema. Expand only: a new table, a
-- unique index and a permission, which the previous version neither reads
-- nor writes, and the retention function replaced by one that deletes a
-- subset of what it deleted.

-- A comment's snapshot is of the comment's company (variance_comment_snapshot).
CREATE UNIQUE INDEX snapshot_company ON cash.snapshot (tenant_id, id, legal_entity_id);

CREATE TABLE cash.variance_comment (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,     -- tenancy context
  period_from      date NOT NULL,
  period_to        date NOT NULL,
  lag              integer NOT NULL CHECK (lag IN (0, 7, 30)),  -- the lags a variance compares at (ADR-0051 §11)
  liquidity_item   text,              -- null: the row of what no rule assigns
  snapshot_id      uuid NOT NULL,     -- the snapshot the variance compared with, lag days before period_from
  text             text NOT NULL,
  status           text NOT NULL CHECK (status IN ('proposed', 'accepted', 'rejected')),
  decided_by       uuid,
  decided_at       timestamptz,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,     -- the author
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, liquidity_item) REFERENCES cash.liquidity_item (tenant_id, code),
  CONSTRAINT variance_comment_snapshot FOREIGN KEY (tenant_id, snapshot_id, legal_entity_id)
    REFERENCES cash.snapshot (tenant_id, id, legal_entity_id),
  CONSTRAINT variance_comment_period CHECK (period_to >= period_from),
  -- 1 to 2,000 characters, not blank: the application refuses a blank text
  -- by a wider idea of space (unicode.IsSpace) first.
  CONSTRAINT variance_comment_text CHECK (char_length(text) BETWEEN 1 AND 2000 AND text ~ '\S'),
  -- Only a proposal is undecided; a decided comment says who decided it, and when.
  CONSTRAINT variance_comment_decided CHECK ((status = 'proposed') = (decided_by IS NULL) AND (decided_by IS NULL) = (decided_at IS NULL))
);
-- The list reads a company's comments newest first; a snapshot's comments
-- are found by the retention function.
CREATE INDEX variance_comment_by_company ON cash.variance_comment (tenant_id, legal_entity_id, created_at, id);
CREATE INDEX variance_comment_by_snapshot ON cash.variance_comment (tenant_id, snapshot_id);
SELECT platform.secure_table('cash.variance_comment'::regclass);

-- Retention (ADR-0051 §6) as 000192 defined it, but a snapshot a variance
-- comment refers to is kept with it, past its 25 months. A comment being
-- written as the snapshot is deleted (read committed: NOT EXISTS does not
-- see it uncommitted) makes the DELETE wait on the comment's lock and fail
-- on variance_comment_snapshot once it commits: the snapshot stays, and the
-- retention activity, retried, finds the comment and skips it.
CREATE OR REPLACE FUNCTION cash.delete_expired_snapshots(entity uuid, on_day date)
RETURNS TABLE (deleted_id uuid, deleted_kind text, deleted_date date)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  IF on_day > current_date + 1 THEN
    RAISE EXCEPTION 'snapshot retention on %: a day after tomorrow', on_day
      USING ERRCODE = 'check_violation', CONSTRAINT = 'snapshot_retention_day';
  END IF;
  RETURN QUERY DELETE FROM cash.snapshot s
    WHERE s.legal_entity_id = entity AND s.snapshot_date < (on_day - interval '25 months')::date
      AND NOT EXISTS (SELECT 1 FROM cash.variance_comment c WHERE c.tenant_id = s.tenant_id AND c.snapshot_id = s.id)
    RETURNING s.id, s.kind, s.snapshot_date;
END $$;

-- Commenting on a variance (ADR-0051 §13, §15): a person's, never an
-- agent's (x-agent: false); reading comments is the variance's own
-- permission, cash.forecast.read.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('cash.variance.comment', 'Comment on the liquidity forecast variance of a company', false);
