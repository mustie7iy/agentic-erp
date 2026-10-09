DROP TABLE IF EXISTS payments.inbox, payments.outbox, payments.bank_file, payments.payment_exclusion, payments.payment_item,
  payments.payment, payments.payment_run, payments.house_bank CASCADE;
DROP FUNCTION IF EXISTS payments.check_item_update();
