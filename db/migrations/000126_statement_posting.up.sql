-- Posting matched statement lines (T49f; ADR-0047; 03-data-model §3.10).
-- A matched line posts to its house bank's main bank account against what
-- its match clears: the payments' lines on the bank's clearing account,
-- cleared with the posting, or a posting rule's account. The posting is
-- dated on the line's booking date, or on a date a person gives with a
-- reason, and waits for an approval request (approvals.BankClearing) unless
-- the company's tier for the line's amount has no steps. Its match then
-- stays: a posted line is not matched again.
ALTER TABLE payments.statement_match
  ADD COLUMN posting_date date,
  ADD COLUMN posting_reason text CHECK (posting_reason <> ''),  -- why a person dated the posting otherwise
  ADD COLUMN approval_request_id uuid,  -- workflow context: the latest request, kept when rejected or withdrawn
  ADD COLUMN journal_entry_year smallint,  -- ledger context: the posting
  ADD COLUMN journal_entry_id uuid,
  ADD CONSTRAINT statement_match_journal CHECK ((journal_entry_id IS NULL) = (journal_entry_year IS NULL)),
  ADD CONSTRAINT statement_match_posted CHECK (journal_entry_id IS NULL OR status = 'matched'),
  -- A posting, or a request for one, has its date; a reason goes with a date.
  ADD CONSTRAINT statement_match_posting_date CHECK (posting_date IS NOT NULL OR (journal_entry_id IS NULL AND approval_request_id IS NULL)),
  ADD CONSTRAINT statement_match_posting_reason CHECK (posting_reason IS NULL OR posting_date IS NOT NULL);
-- One match per posting.
CREATE UNIQUE INDEX statement_match_journal_entry ON payments.statement_match (tenant_id, journal_entry_year, journal_entry_id)
  WHERE journal_entry_id IS NOT NULL;

-- Posting a matched line, and approving the posting: an approval
-- permission, which agents never hold.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('payments.statement.post', 'Post matched bank statement lines, or ask for the approval of their posting', false),
  ('payments.statement.clearing.approve', 'Approve the posting of matched bank statement lines', true);
