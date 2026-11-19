REVOKE UPDATE (valid_from, valid_to, next_review_on) ON credit.credit_limit FROM erp_app;
ALTER TABLE credit.credit_limit
  DROP CONSTRAINT credit_limit_validity_check,
  DROP COLUMN next_review_on,
  DROP COLUMN valid_to,
  DROP COLUMN valid_from;
