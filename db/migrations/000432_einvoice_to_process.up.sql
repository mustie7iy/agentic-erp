-- migrate:no-transaction
-- T65e (ADR-0061 §4, §6): a fetch processes its connector's received
-- documents, oldest first, whether not yet processed or processed and not
-- yet entered: tax invoices since T65d, tax credit notes since T65e, which
-- T65d left received. Every processed document still received is one of
-- the two, so the condition is the status alone; it serves the previous
-- version's condition too, which implies it, so einvoice_to_enter, tax
-- invoices only, is dropped after it (000433). Built concurrently (06
-- §6.4); a failed build leaves an invalid index, dropped by hand before
-- the retry.
CREATE INDEX CONCURRENTLY einvoice_to_process ON einvoicing.einvoice (tenant_id, connector_id, received_at, id)
  WHERE status = 'received';
