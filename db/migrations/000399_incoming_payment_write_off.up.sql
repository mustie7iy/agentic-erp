-- Payment-difference reason codes from configuration (T75o; ADR-0063 §6 As
-- built; 03-data-model §3.26): a company's reasons
-- (companies[].receivables.payment_difference_reasons) say whether what an
-- item is paid short stays open as a residual item (T75f's columns) or is
-- written off to the reason's account. A write-off is a debit line of the
-- payment's clearing entry: write_off_line_no names it,
-- write_off_account_id is the reason's account (ledger context),
-- write_off_amount what was written off, in the payment's currency, and
-- write_off_reason (with an optional free text) the reason's code.
--
-- Expand only (06 §6.4): nullable columns and a check, which the previous
-- version neither reads nor writes.
ALTER TABLE receivables.incoming_payment_item
  ADD COLUMN write_off_amount bigint,
  ADD COLUMN write_off_line_no integer,
  ADD COLUMN write_off_account_id uuid,
  ADD COLUMN write_off_reason text CONSTRAINT incoming_payment_item_write_off_reason_length CHECK (length(write_off_reason) BETWEEN 1 AND 40),
  ADD COLUMN write_off_reason_text text CONSTRAINT incoming_payment_item_write_off_text_length CHECK (length(write_off_reason_text) BETWEEN 1 AND 200),
  -- A write-off is the amount, its line, its account and its reason
  -- together, on an item the customer owed (a debit), more than zero and
  -- less than it, and never beside a residual item of the same item.
  ADD CONSTRAINT incoming_payment_item_write_off CHECK (
    (write_off_amount IS NULL) = (write_off_line_no IS NULL) AND (write_off_amount IS NULL) = (write_off_account_id IS NULL)
    AND (write_off_amount IS NULL) = (write_off_reason IS NULL) AND (write_off_reason_text IS NULL OR write_off_amount IS NOT NULL)
    AND (write_off_amount IS NULL OR (write_off_amount > 0 AND write_off_amount < amount AND write_off_line_no > 0 AND residual_amount IS NULL)));
