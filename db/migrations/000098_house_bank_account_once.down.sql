-- Dropping the constraint drops its index; 000097's down then finds none.
ALTER TABLE payments.house_bank DROP CONSTRAINT house_bank_account_once;
