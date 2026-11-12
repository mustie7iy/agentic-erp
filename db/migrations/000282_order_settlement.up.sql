-- An order settled and closed (T60e; ADR-0064 §3, plan-breadth-week1
-- §T60e). A settlement moves the order's unsettled balance, per account,
-- up to a fiscal period to the receiver of its settlement rule: one
-- journal entry through the ledger's posting port (source context
-- controlling, which the journal takes since 000280), kept here with the
-- period, the receiver, the entry and the amount moved. The receiver and the entry are the ledger's: no
-- foreign key crosses a schema. Insert-only: the table has no version, so
-- the application is granted no UPDATE (platform.secure_table) and never
-- a DELETE; a settlement is not reversed in the first version.
-- A settled order is closed: the third status of an order, from released
-- alone and for good; a closed order takes no posting (the ledger asks
-- controlling.CostObjects). Expand only: a new table and a new status,
-- which the previous version neither writes nor reads as postable.
CREATE TABLE controlling.order_settlement (
  tenant_id                uuid NOT NULL,
  id                       uuid NOT NULL,
  internal_order_id        uuid NOT NULL,
  legal_entity_id          uuid NOT NULL,     -- tenancy context
  fiscal_year              smallint NOT NULL CHECK (fiscal_year BETWEEN 1900 AND 2999),
  fiscal_period            smallint NOT NULL CHECK (fiscal_period BETWEEN 1 AND 16),
  posting_date             date NOT NULL,
  receiver_cost_center_id  uuid NOT NULL,     -- ledger context
  journal_entry_id         uuid NOT NULL,     -- ledger context
  document_number          text NOT NULL CHECK (document_number <> ''),
  amount                   bigint NOT NULL,   -- local currency, minor units: the order's balance moved, debit positive
  account_count            integer NOT NULL CHECK (account_count > 0),
  created_at               timestamptz NOT NULL DEFAULT now(),
  created_by               uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT order_settlement_order FOREIGN KEY (tenant_id, internal_order_id) REFERENCES controlling.internal_order (tenant_id, id),
  -- An entry is one settlement's.
  CONSTRAINT order_settlement_entry_once UNIQUE (tenant_id, journal_entry_id)
);
-- An order's settlements, latest first (GET /v1/internal-orders/{id}/settlements).
CREATE INDEX order_settlement_by_order ON controlling.order_settlement (tenant_id, internal_order_id, id);
SELECT platform.secure_table('controlling.order_settlement'::regclass);

ALTER TABLE controlling.internal_order DROP CONSTRAINT internal_order_status_check;
ALTER TABLE controlling.internal_order ADD CONSTRAINT internal_order_status_check CHECK (status IN ('created', 'released', 'closed'));

-- An order is created in status created, keeps its company and code, is
-- released once and closed once, from released: lines posted to it name
-- it by id and were let in because it was released, so a release is never
-- taken back, and a closed order stays closed.
CREATE OR REPLACE FUNCTION controlling.check_internal_order() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'created' THEN
      RAISE EXCEPTION 'internal order % starts as created', NEW.id USING ERRCODE = 'check_violation', CONSTRAINT = 'internal_order_starts_created';
    END IF;
    RETURN NEW;
  END IF;
  IF (NEW.legal_entity_id, NEW.code) IS DISTINCT FROM (OLD.legal_entity_id, OLD.code) THEN
    RAISE EXCEPTION 'internal order % keeps its company and code', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'internal_order_identity_kept';
  END IF;
  IF OLD.status = 'released' AND NEW.status NOT IN ('released', 'closed') THEN
    RAISE EXCEPTION 'internal order % stays released', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'internal_order_stays_released';
  END IF;
  IF OLD.status = 'closed' AND NEW.status <> 'closed' THEN
    RAISE EXCEPTION 'internal order % stays closed', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'internal_order_stays_closed';
  END IF;
  IF NEW.status = 'closed' AND OLD.status = 'created' THEN
    RAISE EXCEPTION 'internal order % is closed from released', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'internal_order_closes_released';
  END IF;
  RETURN NEW;
END $$;

-- Settling an order posts its costs to its receiver, and closing it ends
-- its postings (ADR-0064 §3): a person's, by permission, with no approval
-- request.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('controlling.order.settle', 'Settle and close internal orders', false);
