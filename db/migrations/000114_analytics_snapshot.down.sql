DROP TABLE analytics.snapshot;
DROP INDEX ledger.journal_entry_posted_xid;
ALTER TABLE ledger.journal_entry DROP COLUMN posted_xid;
