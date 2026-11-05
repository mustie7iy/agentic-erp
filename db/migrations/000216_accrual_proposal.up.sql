-- Accrual proposals (T64e; ADR-0058 §5, §8; 03-data-model §3.22): the
-- close agent proposes, in its running session, one expense of a period
-- close's period that the ledger does not show yet, and the closing
-- context parks its journal entry through the ledger as the agent (Dr the
-- expense account, Cr the company's accrual account, dated the period's
-- last day) for a person to post, edit or cancel there. A proposal keeps
-- its accounts' numbers and names and its centers' codes and names as
-- they were when proposed, and its evidence the documents it rests on
-- with their numbers (a posted invoice, a released order and a posted
-- entry keep theirs). It starts proposed; T64f's :reject moves it to
-- rejected with who decided, when and why, and nothing else of it
-- changes. The accounts, centers, supplier, documents, parked entry,
-- session and principals are other contexts': no foreign key leaves the
-- schema. Every write on a proposal first reads its close FOR UPDATE
-- (ADR-0058 §5), so the duplicate rule (one proposal of a close per
-- expense account, cost center and supplier that is not rejected) is the
-- application's under that lock: a proposal whose parked entry the ledger
-- cancelled counts as rejected (T64f), which no constraint here can see.
-- Expand only: new tables and a permission, which the previous version
-- neither reads nor writes.
CREATE TABLE closing.accrual_proposal (
  tenant_id               uuid NOT NULL,
  id                      uuid NOT NULL,
  close_id                uuid NOT NULL,
  session_id              uuid NOT NULL,          -- agents context: the agent's session
  agent_id                uuid NOT NULL,          -- identity context: the agent
  account_id              uuid NOT NULL,          -- ledger context: the expense account
  account_number          text NOT NULL CHECK (btrim(account_number) <> ''),
  account_name            text NOT NULL,
  accrual_account_id      uuid NOT NULL,          -- ledger context: the company's accrual account
  accrual_account_number  text NOT NULL CHECK (btrim(accrual_account_number) <> ''),
  accrual_account_name    text NOT NULL,
  cost_center_id          uuid,                   -- ledger context
  cost_center_code        text CONSTRAINT accrual_proposal_cost_center_code CHECK (btrim(cost_center_code) <> ''),
  cost_center_name        text,
  profit_center_id        uuid,                   -- ledger context
  profit_center_code      text CONSTRAINT accrual_proposal_profit_center_code CHECK (btrim(profit_center_code) <> ''),
  profit_center_name      text,
  supplier_id             uuid,                   -- partner context
  currency                text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  amount                  bigint NOT NULL,        -- minor units of the company's local currency
  description             text NOT NULL CHECK (btrim(description) <> '' AND char_length(description) <= 200),
  reason                  text NOT NULL CHECK (btrim(reason) <> '' AND char_length(reason) <= 2000),
  parked_entry_id         uuid NOT NULL,          -- ledger context: ledger.parked_entry
  posting_date            date NOT NULL,          -- the parked entry's, the period's last day
  status                  text NOT NULL CHECK (status IN ('proposed', 'rejected')),
  decided_by              uuid,                   -- identity context: a person
  decided_at              timestamptz,
  decision_reason         text CHECK (btrim(decision_reason) <> '' AND char_length(decision_reason) <= 2000),
  version                 integer NOT NULL DEFAULT 1,
  created_at              timestamptz NOT NULL DEFAULT now(),
  created_by              uuid NOT NULL,
  updated_at              timestamptz NOT NULL DEFAULT now(),
  updated_by              uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, close_id) REFERENCES closing.period_close (tenant_id, id),
  -- An accrual is an expense the period bore: more than zero (ADR-0058 §5).
  CONSTRAINT accrual_proposal_amount CHECK (amount > 0),
  -- A center's code and name are kept exactly with the center.
  CONSTRAINT accrual_proposal_centers CHECK ((cost_center_id IS NULL) = (cost_center_code IS NULL) AND (cost_center_id IS NULL) = (cost_center_name IS NULL)
    AND (profit_center_id IS NULL) = (profit_center_code IS NULL) AND (profit_center_id IS NULL) = (profit_center_name IS NULL)),
  -- Who decided it, when and why are set exactly once it is rejected.
  CONSTRAINT accrual_proposal_decided CHECK ((status = 'proposed') = (decided_by IS NULL) AND (status = 'proposed') = (decided_at IS NULL)
    AND (status = 'proposed') = (decision_reason IS NULL)),
  -- One proposal per parked entry: the entry names the proposal it was parked for.
  CONSTRAINT accrual_proposal_parked_once UNIQUE (tenant_id, parked_entry_id)
);
-- The duplicate rule reads a close's proposals of an account; the list
-- pages by creation and filters by session.
CREATE INDEX accrual_proposal_by_close ON closing.accrual_proposal (tenant_id, close_id, account_id);
CREATE INDEX accrual_proposal_by_created ON closing.accrual_proposal (tenant_id, created_at, id);
CREATE INDEX accrual_proposal_by_session ON closing.accrual_proposal (tenant_id, session_id);
CREATE TRIGGER accrual_proposal_touch BEFORE UPDATE ON closing.accrual_proposal FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- A proposal starts proposed and moves only to rejected, with its
-- decision; nothing else of it changes.
CREATE FUNCTION closing.check_accrual_proposal() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  changing text[] := ARRAY['status', 'decided_by', 'decided_at', 'decision_reason', 'version', 'updated_at', 'updated_by'];
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'proposed' THEN
      RAISE EXCEPTION 'an accrual proposal starts proposed, not %', NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'accrual_proposal_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF to_jsonb(NEW) - changing IS DISTINCT FROM to_jsonb(OLD) - changing THEN
    RAISE EXCEPTION 'accrual proposal % changes only its status and decision', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'accrual_proposal_fixed';
  END IF;
  IF (OLD.status, NEW.status) IS DISTINCT FROM ('proposed', 'rejected') THEN
    RAISE EXCEPTION 'accrual proposal % does not move from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'accrual_proposal_transition';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION closing.check_accrual_proposal() OWNER TO erp_owner;
CREATE TRIGGER accrual_proposal_transition BEFORE INSERT OR UPDATE ON closing.accrual_proposal FOR EACH ROW EXECUTE FUNCTION closing.check_accrual_proposal();
SELECT platform.secure_table('closing.accrual_proposal'::regclass);

-- The documents a proposal rests on, 1-10, each once, in the order given:
-- a supplier invoice or a purchase order (payables and procurement
-- contexts) with its supplier, or a journal entry (ledger context) with
-- its fiscal year, each with its document number. Written with the
-- proposal, never changed.
CREATE TABLE closing.accrual_evidence (
  tenant_id        uuid NOT NULL,
  proposal_id      uuid NOT NULL,
  seq              smallint NOT NULL CHECK (seq BETWEEN 1 AND 10),
  kind             text NOT NULL CHECK (kind IN ('supplier_invoice', 'purchase_order', 'journal_entry')),
  document_id      uuid NOT NULL,
  document_number  text NOT NULL CHECK (btrim(document_number) <> ''),
  fiscal_year      smallint,
  supplier_id      uuid,                          -- partner context
  PRIMARY KEY (tenant_id, proposal_id, seq),
  CONSTRAINT accrual_evidence_once UNIQUE (tenant_id, proposal_id, kind, document_id),
  FOREIGN KEY (tenant_id, proposal_id) REFERENCES closing.accrual_proposal (tenant_id, id),
  -- A journal entry is keyed by its fiscal year and has no supplier; an
  -- invoice or an order has its supplier and no fiscal year.
  CONSTRAINT accrual_evidence_kind_keys CHECK ((kind = 'journal_entry') = (fiscal_year IS NOT NULL) AND (kind = 'journal_entry') = (supplier_id IS NULL))
);
CREATE TRIGGER accrual_evidence_immutable BEFORE UPDATE OR DELETE ON closing.accrual_evidence FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('closing.accrual_evidence'::regclass);

-- Proposing accruals (agents) (ADR-0058 §13); parking their entries also
-- needs ledger.entry.park.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('closing.accrual.propose', 'Propose accruals for a period close, parked as journal entries for people to post (agents)', false);
