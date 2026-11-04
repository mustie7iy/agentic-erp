-- Variance comments (T64i; ADR-0058 §5, §10; 03-data-model §3.22): a
-- comment explains one row (an account) of one basis of a period close's
-- variance report. The close agent proposes one in its running session
-- (proposed), which a person accepts, with the text as it is or replaced
-- (edited: the proposal's text kept as proposed_text), or rejects with a
-- reason; a person's own comment is accepted at once. Accepting or writing
-- one sets the row's comment accepted before it superseded, so a row has at
-- most one accepted comment. Each comment keeps the row's actual and
-- comparison when it was written, and the account's number and name: it
-- is stale once the row's figures differ. Its citations are the journal entries it rests on, each with a
-- line on the account in the close's period or the period compared
-- against, with their document number and posting date as they were cited
-- (a posted entry never changes). account_id, the entries, the session and
-- the principals are other contexts': no foreign key leaves the schema.
-- Every write on a comment first reads its close FOR UPDATE (ADR-0058 §5).
-- Expand only: new tables and permissions, which the previous version
-- neither reads nor writes.
CREATE TABLE closing.variance_comment (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  close_id         uuid NOT NULL,
  basis            text NOT NULL CHECK (basis ~ '^[a-z_]{1,32}$'),
  account_id       uuid NOT NULL,              -- ledger context
  account_number   text NOT NULL CHECK (btrim(account_number) <> ''),
  account_name     text NOT NULL,              -- as it was when the comment was written
  text             text NOT NULL CHECK (btrim(text) <> '' AND char_length(text) <= 2000),
  proposed_text    text CHECK (btrim(proposed_text) <> '' AND char_length(proposed_text) <= 2000),
  author_kind      text NOT NULL CHECK (author_kind IN ('human', 'agent')),
  author_id        uuid NOT NULL,              -- identity context
  session_id       uuid,                       -- agents context: the agent's session
  currency         text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  actual           bigint NOT NULL,            -- the row's figures when it was written, minor units
  comparison       bigint NOT NULL,
  status           text NOT NULL CHECK (status IN ('proposed', 'accepted', 'rejected', 'superseded')),
  decided_by       uuid,                       -- identity context: a person
  decided_at       timestamptz,
  decision_reason  text CHECK (btrim(decision_reason) <> '' AND char_length(decision_reason) <= 2000),
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, close_id) REFERENCES closing.period_close (tenant_id, id),
  -- An agent's comment is made in its session; a person's in none.
  CONSTRAINT variance_comment_session CHECK ((author_kind = 'agent') = (session_id IS NOT NULL)),
  -- Who decided it and when are set exactly once it is decided; a reason
  -- exactly when it was rejected.
  CONSTRAINT variance_comment_decided CHECK ((status = 'proposed') = (decided_by IS NULL) AND (status = 'proposed') = (decided_at IS NULL)
    AND (status = 'rejected') = (decision_reason IS NOT NULL)),
  -- A proposal's text is kept when the person accepting it replaced it.
  CONSTRAINT variance_comment_edited CHECK (proposed_text IS NULL OR author_kind = 'agent' AND status IN ('accepted', 'superseded'))
);
-- One accepted comment per close, basis and row (ADR-0058 §5).
CREATE UNIQUE INDEX variance_comment_accepted_once ON closing.variance_comment (tenant_id, close_id, basis, account_id) WHERE status = 'accepted';
-- One proposal per session and row (ADR-0058 §10).
CREATE UNIQUE INDEX variance_comment_proposed_once ON closing.variance_comment (tenant_id, close_id, basis, account_id, session_id)
  WHERE session_id IS NOT NULL;
-- The report and the check read a close's comments of a basis; the list
-- pages by creation.
CREATE INDEX variance_comment_by_row ON closing.variance_comment (tenant_id, close_id, basis, account_id);
CREATE INDEX variance_comment_by_created ON closing.variance_comment (tenant_id, created_at, id);
CREATE TRIGGER variance_comment_touch BEFORE UPDATE ON closing.variance_comment FOR EACH ROW EXECUTE FUNCTION platform.touch();

-- An agent's comment starts proposed and a person's accepted; a proposal
-- moves to accepted (its text then replaceable) or rejected, an accepted
-- comment to superseded. Nothing else of it changes.
CREATE FUNCTION closing.check_variance_comment() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  changing text[] := ARRAY['status', 'text', 'proposed_text', 'decided_by', 'decided_at', 'decision_reason', 'version', 'updated_at', 'updated_by'];
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> (CASE NEW.author_kind WHEN 'agent' THEN 'proposed' ELSE 'accepted' END) OR NEW.proposed_text IS NOT NULL THEN
      RAISE EXCEPTION 'an agent''s variance comment starts proposed and a person''s accepted, not %', NEW.status
        USING ERRCODE = 'check_violation', CONSTRAINT = 'variance_comment_transition';
    END IF;
    RETURN NEW;
  END IF;
  IF to_jsonb(NEW) - changing IS DISTINCT FROM to_jsonb(OLD) - changing THEN
    RAISE EXCEPTION 'variance comment % changes only its status and decision', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'variance_comment_fixed';
  END IF;
  IF NEW.status IS DISTINCT FROM OLD.status AND (OLD.status, NEW.status) NOT IN (('proposed', 'accepted'), ('proposed', 'rejected'), ('accepted', 'superseded')) THEN
    RAISE EXCEPTION 'variance comment % does not move from % to %', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'check_violation', CONSTRAINT = 'variance_comment_transition';
  END IF;
  -- The text and the decision are written when it is decided, then kept.
  IF (OLD.status, NEW.status) NOT IN (('proposed', 'accepted'), ('proposed', 'rejected'))
    AND (NEW.text, NEW.proposed_text, NEW.decided_by, NEW.decided_at, NEW.decision_reason)
      IS DISTINCT FROM (OLD.text, OLD.proposed_text, OLD.decided_by, OLD.decided_at, OLD.decision_reason) THEN
    RAISE EXCEPTION 'variance comment % keeps its text and decision', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'variance_comment_transition';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION closing.check_variance_comment() OWNER TO erp_owner;
CREATE TRIGGER variance_comment_transition BEFORE INSERT OR UPDATE ON closing.variance_comment FOR EACH ROW EXECUTE FUNCTION closing.check_variance_comment();
SELECT platform.secure_table('closing.variance_comment'::regclass);

-- The journal entries a comment cites (ledger context), at most 20, each
-- once, with its document number and posting date. Written with the
-- comment, never changed.
CREATE TABLE closing.variance_citation (
  tenant_id           uuid NOT NULL,
  comment_id          uuid NOT NULL,
  seq                 smallint NOT NULL CHECK (seq BETWEEN 1 AND 20),
  journal_entry_year  smallint NOT NULL,
  journal_entry_id    uuid NOT NULL,           -- ledger.journal_entry (tenant_id, fiscal_year, id)
  document_number     text NOT NULL CHECK (btrim(document_number) <> ''),
  posting_date        date NOT NULL,
  PRIMARY KEY (tenant_id, comment_id, seq),
  CONSTRAINT variance_citation_once UNIQUE (tenant_id, comment_id, journal_entry_id),
  FOREIGN KEY (tenant_id, comment_id) REFERENCES closing.variance_comment (tenant_id, id)
);
CREATE TRIGGER variance_citation_immutable BEFORE UPDATE OR DELETE ON closing.variance_citation FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('closing.variance_citation'::regclass);

-- Proposing variance comments (agents) and writing and deciding them
-- (people) (ADR-0058 §13).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('closing.variance.propose', 'Propose comments explaining a period close''s variances (agents)', false),
  ('closing.variance.comment', 'Write, accept and reject comments explaining a period close''s variances', false);
