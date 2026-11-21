-- A plant's region (product owner 2026-11-21; ADR-0082 As built S2j, the
-- plant on the order; ADR-0063 As built, the emirate on the item): the
-- country subdivision of the plant's address, an ISO 3166-2 code (a UAE
-- plant's emirate, AE-DU), as SAP keeps a plant's region on its address
-- (T001W-REGIO, from memory). A UAE sales order's stock item defaults its
-- emirate of supply to its plant's: the VAT 201 reports a sale by the
-- emirate of the supplier's establishment most closely connected with it
-- (ADR-0049 §11). Only a plant has one, in that form; that it is a
-- subdivision of its company's country is the tenancy context's check.
-- Expand only (06 §6.4): a nullable column, null on every existing unit,
-- which the previous version neither reads nor writes, and a check it
-- always passes.
ALTER TABLE tenancy.org_unit
  ADD COLUMN region text,
  ADD CONSTRAINT org_unit_region CHECK (region IS NULL OR (kind = 'plant' AND region ~ '^[A-Z]{2}-[A-Z0-9]{1,3}$'));
