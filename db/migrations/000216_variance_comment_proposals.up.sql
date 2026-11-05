-- The forecast agent's variance comment proposals (T53i; ADR-0051 §11,
-- §13, §15; 03-data-model §3.18): an agent proposes a comment in its
-- running session (session_id, the agents context's: no foreign key
-- crosses a schema), citing the documents behind the attributed variance
-- it explains (citations, each checked against the variance when it is
-- proposed); a person accepts it, as it stands or with their own text
-- (the agent's kept as proposed_text), or rejects it with a reason, once.
-- A decision changes only the status, who decided, when, the reason, the
-- text on an edited acceptance and the version (ADR-0008), so the
-- application is granted UPDATE of those columns alone, and a trigger
-- lets a comment be decided only while proposed, and its text change only
-- as an accepted edit that keeps the agent's. Expand only: new columns, each with a default or null, that
-- the previous version neither reads nor writes and whose checks its
-- comments (accepted as written, without a session) meet; a trigger on
-- updates, which the previous version never makes; and a permission.

ALTER TABLE cash.variance_comment
  ADD COLUMN session_id      uuid,  -- agents context: the session that proposed it
  ADD COLUMN citations       jsonb NOT NULL DEFAULT '[]',  -- the documents a proposal cites ({kind, id})
  ADD COLUMN decision_reason text,  -- why a person rejected a proposal
  ADD COLUMN proposed_text   text,  -- the agent's text, when a person accepted it edited
  ADD COLUMN version         integer NOT NULL DEFAULT 1,
  -- A proposal names its session; only an agent's comment cites documents.
  ADD CONSTRAINT variance_comment_session CHECK ((status <> 'proposed' OR session_id IS NOT NULL)
    AND (session_id IS NOT NULL OR citations = '[]'::jsonb)),
  ADD CONSTRAINT variance_comment_citations CHECK (jsonb_typeof(citations) = 'array' AND jsonb_array_length(citations) <= 50),
  -- A rejected proposal says why; nothing else carries a reason.
  ADD CONSTRAINT variance_comment_rejected CHECK ((status = 'rejected') = (decision_reason IS NOT NULL)
    AND (decision_reason IS NULL OR char_length(decision_reason) BETWEEN 1 AND 2000)),
  ADD CONSTRAINT variance_comment_version CHECK (version >= 1),
  -- Only an accepted proposal was edited, and then to another text.
  ADD CONSTRAINT variance_comment_edited CHECK (proposed_text IS NULL
    OR (status = 'accepted' AND session_id IS NOT NULL AND proposed_text <> text AND char_length(proposed_text) BETWEEN 1 AND 2000));

-- A comment is decided once, from proposed, by a person other than its
-- author (the agent that proposed it), raising its version by one.
CREATE FUNCTION cash.check_variance_comment_decision() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'proposed' OR NEW.status = 'proposed' THEN
    RAISE EXCEPTION 'variance comment %: only a proposal is decided, once', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'variance_comment_decided_once';
  END IF;
  IF NEW.version <> OLD.version + 1 THEN
    RAISE EXCEPTION 'variance comment %: a decision raises the version by one', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'variance_comment_decision_version';
  END IF;
  IF (NEW.text IS DISTINCT FROM OLD.text OR NEW.proposed_text IS NOT NULL)
      AND (NEW.status <> 'accepted' OR NEW.proposed_text IS DISTINCT FROM OLD.text) THEN
    RAISE EXCEPTION 'variance comment %: only an accepted edit changes the text, keeping the agent''s', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'variance_comment_edit_keeps_proposal';
  END IF;
  IF NEW.decided_by = OLD.created_by THEN
    RAISE EXCEPTION 'variance comment %: its author does not decide its own proposal', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'variance_comment_decided_by_other';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER variance_comment_decision BEFORE UPDATE ON cash.variance_comment
  FOR EACH ROW EXECUTE FUNCTION cash.check_variance_comment_decision();

GRANT UPDATE (status, decided_by, decided_at, decision_reason, text, proposed_text, version) ON cash.variance_comment TO erp_app;

-- Proposing a variance comment (ADR-0051 §13, §15): the forecast agent's,
-- x-autonomy propose; deciding one is cash.variance.comment, a person's.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('cash.variance.propose', 'Propose comments on the liquidity forecast variance for a person to decide (the forecast agent)', false);
