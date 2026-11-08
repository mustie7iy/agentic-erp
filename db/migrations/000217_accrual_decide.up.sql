-- Deciding accrual proposals (T64f; ADR-0058 §8, §13; 03-data-model
-- §3.22): a person rejects the close agent's proposal, its parked entry
-- cancelled in the ledger, which needs ledger.entry.park also. A
-- proposal's other statuses (pending approval, posted, reversed, and
-- rejected when the ledger cancelled or rejected its entry) are derived
-- at the read from the parked entry through the ledger's port, never
-- stored: accrual_proposal keeps proposed and rejected (000216).
-- Expand only: a new permission, which the previous version never checks.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('closing.accrual.decide', 'Reject the close agent''s accrual proposals, cancelling their parked entries', false);
