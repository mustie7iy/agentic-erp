-- migrate:no-transaction
-- review:batch90 (ADR-0065 As built T86d, tracker leaf "A dispute case
-- disputes at most what is still owed of an item"): opening a case reads
-- what resolved cases accepted of each of its items, by journal line, and
-- dispute_case_item_one_open covers only open items. Built concurrently
-- (06 §6.4); a failed build leaves an invalid index, dropped by hand
-- before the retry. Partial: only an item something was accepted of is
-- read.
CREATE INDEX CONCURRENTLY dispute_case_item_accepted_by_line ON collections.dispute_case_item (tenant_id, journal_entry_id, line_no)
  WHERE accepted_amount > 0;
