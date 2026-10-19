-- migrate:no-transaction
-- The index house_bank_account_once (000098) is attached to: at most one
-- active house bank of a legal entity per ledger account (ADR-0038). Nulls
-- (inactive banks) are distinct. Built concurrently (06 §6.4); no IF NOT
-- EXISTS, so an invalid index left by a failed build (two active banks on
-- one account) is dropped by hand once they are fixed.
CREATE UNIQUE INDEX CONCURRENTLY house_bank_account_once ON payments.house_bank (tenant_id, legal_entity_id, active_gl_account_id);
