-- Validates what 000385 added NOT VALID (06 §6.4).
ALTER TABLE controlling.allocation_cycle VALIDATE CONSTRAINT allocation_cycle_share_basis;
ALTER TABLE controlling.allocation_cycle VALIDATE CONSTRAINT allocation_cycle_key_figure;
ALTER TABLE controlling.allocation_cycle VALIDATE CONSTRAINT allocation_cycle_key_figure_company;
