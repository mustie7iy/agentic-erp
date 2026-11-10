-- The translation part of a company's group-currency revaluation
-- differences in a consolidation run (ADR-0059 §7, As built: the
-- group-currency revaluation's translation part; ADR-0042, the
-- group-currency difference in consolidation; the accounting specialist's
-- B4 correction 1, po:2026-11-09; IAS 21.39(c)): a revaluation run values
-- group currency on its own, so the group amount its entries book on a
-- profit and loss account is not the local amount translated. The run
-- keeps the local amount translated in the profit and takes the rest to
-- the company's translation reserve; this is the fiscal year's amount
-- through the period, in the group currency's minor units, debit
-- positive, which its profit and loss shows as part of other
-- comprehensive income. Recorded with the company's translation, so a
-- statement reads it with the members it reads already and no row of
-- run_balance. Expand only: a column with a default the previous version
-- neither reads nor writes; a company it translated moved nothing, which
-- zero says.
ALTER TABLE consolidation.run_member ADD COLUMN revaluation_translation_gc bigint NOT NULL DEFAULT 0;
-- It is part of the translation: none before it, and run_member_translated_once
-- (000224) lets no column change after it.
ALTER TABLE consolidation.run_member ADD CONSTRAINT run_member_revaluation_translation
  CHECK (revaluation_translation_gc = 0 OR translated_at IS NOT NULL);
GRANT UPDATE (revaluation_translation_gc) ON consolidation.run_member TO erp_app;
