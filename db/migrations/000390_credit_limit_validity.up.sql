-- A credit limit's validity and its next review (T77c; ADR-0066 As built
-- T77c; 03-data-model §3.28): the limit is used by the credit check only
-- on the company's days from valid_from to valid_to, both included, either
-- open (NULL: no start, no end); next_review_on is the day a credit manager
-- plans to look at it again, which the worklist lists it by.
-- Expand only (06 §6.4): three nullable columns the previous version
-- neither reads nor writes; NULL in each is what every limit set before
-- is, valid without a start or an end and with no review planned.
ALTER TABLE credit.credit_limit
  ADD COLUMN valid_from date,
  ADD COLUMN valid_to date,
  ADD COLUMN next_review_on date,
  ADD CONSTRAINT credit_limit_validity_check CHECK (valid_to >= valid_from);
-- A change sets the amount, the validity and the review date: the
-- customer, the company and the currency of a limit stay.
GRANT UPDATE (valid_from, valid_to, next_review_on) ON credit.credit_limit TO erp_app;
