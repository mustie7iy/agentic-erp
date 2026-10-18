-- The replacements the model gateway made in a call's text before it left
-- the cell (T42e, ADR-0036: IBANs, card numbers, tax identifiers): a
-- count, never what was replaced. Calls recorded before have none.
ALTER TABLE agents.model_call ADD COLUMN redactions integer NOT NULL DEFAULT 0
  CONSTRAINT model_call_redactions_check CHECK (redactions >= 0);
