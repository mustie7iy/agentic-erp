-- An agent's proposals for statement lines (T49g; ADR-0047; 03-data-model
-- §3.10). The reconciliation agent proposes, for a line the rules left
-- unmatched or open, the payments it clears, a posting rule, or its
-- exclusion, with a reason, in one of its sessions (agents context). The
-- proposal is a live match of kind agent, open until a person decides it:
-- accepted (the person's own match by it, which the person then posts as
-- any match by hand), rejected (the line is unmatched again) or overridden
-- (the person matched or excluded the line otherwise). It reserves no
-- payment and never posts.
ALTER TABLE payments.statement_match
  ADD COLUMN session_id uuid,  -- agents context: the session whose agent proposed it
  ADD COLUMN proposal text CHECK (proposal IN ('match', 'exclude')),  -- what the agent proposes
  ADD COLUMN outcome text CHECK (outcome IN ('accepted', 'rejected', 'overridden')),  -- what a person did with the proposal
  -- An agent's match names its session and what it proposes; no other does.
  ADD CONSTRAINT statement_match_agent CHECK ((kind = 'agent') = (session_id IS NOT NULL) AND (kind = 'agent') = (proposal IS NOT NULL)),
  -- An agent's match stays open until a person decides it, and never posts.
  ADD CONSTRAINT statement_match_agent_open CHECK (kind <> 'agent' OR (status IN ('open', 'rejected') AND posting_date IS NULL)),
  -- A proposal a person decided records the outcome; nothing else has one.
  ADD CONSTRAINT statement_match_outcome CHECK ((kind = 'agent' AND status = 'rejected') = (outcome IS NOT NULL));
-- A session proposes once per line: the same proposal again finds it.
CREATE UNIQUE INDEX statement_match_session ON payments.statement_match (tenant_id, session_id, statement_id, line_no) WHERE session_id IS NOT NULL;

-- Proposing: what the reconciliation agent holds besides reading
-- statements. Deciding a proposal is payments.statement.match, a person's.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('payments.statement.propose', 'Propose matches or exclusions of bank statement lines for a person to decide (the reconciliation agent)', false);
