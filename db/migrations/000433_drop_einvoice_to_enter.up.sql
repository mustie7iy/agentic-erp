-- migrate:no-transaction
-- T65e (ADR-0061 §4): einvoice_to_enter (000425) listed processed tax
-- invoices still received; the fetch now lists every received document by
-- einvoice_to_process (000432), which serves this version's condition and
-- the previous one's, so the narrower index only costs writes. Contract
-- step of 000432 (06 §6.4).
DROP INDEX CONCURRENTLY IF EXISTS einvoicing.einvoice_to_enter;
