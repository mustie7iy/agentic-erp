-- Memo records (T53d; ADR-0051 §7, §13, §15; 03-data-model §3.18): the
-- expected receipts and payments people enter, upload or accept from an
-- agent's proposal, until receivables exist (decision P6). A record counts
-- in the cash position on its house bank and in the liquidity forecast
-- (T53e) while it is active and not expired; a series is expanded at read
-- time, each occurrence expiring as many days after its own date as the
-- first after the value date, and an override (recurrence_of,
-- occurrence_date) replaces one occurrence. legal_entity_id is the tenancy
-- context's, house_bank_id the payments context's, the statement lines the
-- payments context's and session_id the agents context's: no foreign key
-- crosses a schema; the cash context checks them through its ports. Expand
-- only: a new table, outbox and permissions, which the previous version
-- neither reads nor writes.
SELECT platform.create_event_tables('cash');
SELECT platform.secure_table('cash.outbox'::regclass);
SELECT platform.secure_table('cash.inbox'::regclass);
GRANT UPDATE (published_at) ON cash.outbox TO erp_app;
GRANT USAGE ON SCHEMA cash TO erp_relay;
GRANT SELECT, UPDATE (published_at) ON cash.outbox TO erp_relay;

CREATE TABLE cash.memo_record (
  tenant_id              uuid NOT NULL,
  id                     uuid NOT NULL,
  legal_entity_id        uuid NOT NULL,     -- tenancy context
  house_bank_id          uuid,              -- payments context; null: forecast only
  currency               char(3) NOT NULL REFERENCES ref.currency,
  amount                 bigint NOT NULL CHECK (amount <> 0),  -- minor units, signed: a receipt positive
  value_date             date NOT NULL,     -- a series' first occurrence
  expires_on             date NOT NULL,
  counts_until           date,              -- the last occurrence's expiry; null: a series without end
  certainty              text NOT NULL CHECK (certainty IN ('confirmed', 'expected', 'planned')),
  liquidity_item         text NOT NULL,
  description            text CHECK (char_length(description) BETWEEN 1 AND 500),
  reference              text CHECK (char_length(reference) BETWEEN 1 AND 100),
  recurrence             text CHECK (recurrence IN ('weekly', 'monthly', 'quarterly')),
  recurrence_until       date,
  recurrence_count       integer CHECK (recurrence_count BETWEEN 2 AND 520),
  recurrence_of          uuid,
  occurrence_date        date,
  status                 text NOT NULL CHECK (status IN ('proposed', 'active', 'realised', 'cancelled', 'rejected')),
  origin                 text NOT NULL CHECK (origin IN ('manual', 'upload', 'agent')),
  session_id             uuid,              -- agents context: the session that proposed it
  reason                 text CHECK (char_length(reason) BETWEEN 1 AND 2000),
  evidence               jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(evidence) = 'array'),  -- the statement lines an agent cites
  batch_key              text CHECK (batch_key ~ '^[A-Za-z0-9._:-]{1,100}$'),
  realised_statement_id  uuid,              -- payments context
  realised_line_no       integer CHECK (realised_line_no >= 1),
  decided_by             uuid,
  decided_at             timestamptz,
  comment                text CHECK (char_length(comment) BETWEEN 1 AND 2000),
  version                integer NOT NULL DEFAULT 1,
  created_at             timestamptz NOT NULL DEFAULT now(),
  created_by             uuid NOT NULL,
  updated_at             timestamptz NOT NULL DEFAULT now(),
  updated_by             uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, liquidity_item) REFERENCES cash.liquidity_item (tenant_id, code),
  FOREIGN KEY (tenant_id, recurrence_of) REFERENCES cash.memo_record (tenant_id, id),
  CONSTRAINT memo_record_expiry CHECK (expires_on >= value_date AND (counts_until IS NULL OR counts_until >= expires_on)),
  -- A one-off record counts until it expires; only a series may count
  -- without end.
  CONSTRAINT memo_record_counts_until CHECK (recurrence IS NOT NULL OR (counts_until IS NOT NULL AND counts_until = expires_on)),
  -- A series ends on a day or after a count, or neither; a one-off record
  -- has no end.
  CONSTRAINT memo_record_recurrence CHECK (num_nonnulls(recurrence_until, recurrence_count) <= 1
    AND (recurrence IS NOT NULL OR num_nonnulls(recurrence_until, recurrence_count) = 0)
    AND (recurrence_until IS NULL OR recurrence_until >= value_date)),
  -- An override names the series and its occurrence, and is one-off.
  CONSTRAINT memo_record_override CHECK ((recurrence_of IS NULL) = (occurrence_date IS NULL) AND (recurrence_of IS NULL OR recurrence IS NULL)),
  CONSTRAINT memo_record_override_once UNIQUE (tenant_id, recurrence_of, occurrence_date),
  -- An agent's proposal names its session and reason; only a proposal is
  -- proposed or rejected.
  CONSTRAINT memo_record_agent CHECK ((origin = 'agent') = (session_id IS NOT NULL) AND (origin = 'agent') = (reason IS NOT NULL)
    AND (status NOT IN ('proposed', 'rejected') OR origin = 'agent')),
  -- An upload's records carry its batch key and a reference, once per key.
  CONSTRAINT memo_record_batch CHECK ((origin = 'upload') = (batch_key IS NOT NULL) AND (batch_key IS NULL OR reference IS NOT NULL)),
  CONSTRAINT memo_record_batch_reference UNIQUE (tenant_id, legal_entity_id, batch_key, reference),
  -- A record is realised by a statement line or by a person, and a series
  -- only through an override of an occurrence.
  CONSTRAINT memo_record_realised CHECK ((realised_statement_id IS NULL) = (realised_line_no IS NULL)
    AND (status = 'realised' OR realised_statement_id IS NULL) AND (status <> 'realised' OR recurrence IS NULL)),
  -- A decision on it records who and when.
  CONSTRAINT memo_record_decided CHECK ((decided_by IS NULL) = (decided_at IS NULL))
);
CREATE TRIGGER memo_record_touch BEFORE UPDATE ON cash.memo_record FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list, the cash position and the forecast read a company's records by
-- value date.
CREATE INDEX memo_record_by_company ON cash.memo_record (tenant_id, legal_entity_id, value_date);
SELECT platform.secure_table('cash.memo_record'::regclass);

-- A record names an active liquidity item whose direction fits its sign,
-- checked when it is written with an item or amount, or opened again
-- (cancelled, then active: an upload's row again): an item becomes
-- inactive only when no open record names it (cash.Setup), and a record
-- realised, cancelled or rejected keeps the item it had. The item's row is
-- locked, so a configuration deactivating it waits for this transaction.
CREATE FUNCTION cash.check_memo_item() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE active boolean; dir text;
BEGIN
  IF TG_OP = 'UPDATE' AND NEW.liquidity_item = OLD.liquidity_item AND NEW.amount = OLD.amount
     AND NOT (OLD.status NOT IN ('proposed', 'active') AND NEW.status IN ('proposed', 'active')) THEN
    RETURN NEW;
  END IF;
  SELECT i.is_active, i.direction INTO active, dir FROM cash.liquidity_item i
  WHERE i.tenant_id = NEW.tenant_id AND i.code = NEW.liquidity_item FOR SHARE;
  IF active IS NOT TRUE THEN
    RAISE EXCEPTION 'memo record %: liquidity item % is not active', NEW.id, NEW.liquidity_item
      USING ERRCODE = 'check_violation', CONSTRAINT = 'memo_record_item_active';
  END IF;
  IF (dir = 'inflow' AND NEW.amount < 0) OR (dir = 'outflow' AND NEW.amount > 0) THEN
    RAISE EXCEPTION 'memo record %: liquidity item % is for % flows', NEW.id, NEW.liquidity_item, dir
      USING ERRCODE = 'check_violation', CONSTRAINT = 'memo_record_item_direction';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION cash.check_memo_item() OWNER TO erp_owner;
CREATE TRIGGER memo_record_item BEFORE INSERT OR UPDATE OF liquidity_item, amount, status ON cash.memo_record
  FOR EACH ROW EXECUTE FUNCTION cash.check_memo_item();

-- Reading, writing and proposing (ADR-0051 §7, §15). Accepting or
-- rejecting an agent's proposal is cash.memo.write, a person's.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('cash.memo.read', 'View memo records of expected receipts and payments', false),
  ('cash.memo.write', 'Create, change, upload, realise and cancel memo records, and decide agents'' proposals', false),
  ('cash.memo.propose', 'Propose memo records for a person to decide (the forecast agent)', false);
