-- The exchange differences on a company's net investments in a
-- consolidation run (ADR-0059 As built: a net investment's exchange
-- differences; the accounting specialist's B4 correction 2,
-- po:2026-11-09; identification per account and trading partner in the
-- group's configuration, po:2026-11-10; IAS 21.32): the differences a
-- company's revaluation runs book on a monetary item designated as part
-- of a net investment in a foreign operation go to other comprehensive
-- income in the consolidated statements, not to profit or loss. The run
-- takes them out of the company's profit and into its translation
-- reserve; this is the fiscal year's amount through the period beyond
-- revaluation_translation_gc (000248), which holds their translation
-- part as every revaluation line's: the lines' local amounts translated,
-- in the group currency's minor units, debit positive, which the profit
-- and loss shows as part of other comprehensive income. Recorded with
-- the company's translation, as 000248's. Expand only: a column with a
-- default the previous version neither reads nor writes; a company it
-- translated moved nothing, which zero says.
ALTER TABLE consolidation.run_member ADD COLUMN net_investment_gc bigint NOT NULL DEFAULT 0;
-- It is part of the translation: none before it, and run_member_translated_once
-- (000224) lets no column change after it.
ALTER TABLE consolidation.run_member ADD CONSTRAINT run_member_net_investment
  CHECK (net_investment_gc = 0 OR translated_at IS NOT NULL);
GRANT UPDATE (net_investment_gc) ON consolidation.run_member TO erp_app;
