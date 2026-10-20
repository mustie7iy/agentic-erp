-- The custom field schema version a row's extensions were validated
-- against (ADR-0046, T58c): the version of the entity's schema in the
-- configuration the writing transaction read. NULL for a row written while
-- the tenant declared no schema for the entity (its extensions are empty
-- then) and for rows written before this migration. A version is immutable
-- once activated, so the number always means the same schema.
ALTER TABLE payables.supplier_invoice ADD COLUMN extensions_schema integer CONSTRAINT supplier_invoice_extensions_schema_check CHECK (extensions_schema >= 1);
ALTER TABLE procurement.purchase_order ADD COLUMN extensions_schema integer CONSTRAINT purchase_order_extensions_schema_check CHECK (extensions_schema >= 1);
ALTER TABLE partner.business_partner ADD COLUMN extensions_schema integer CONSTRAINT business_partner_extensions_schema_check CHECK (extensions_schema >= 1);
