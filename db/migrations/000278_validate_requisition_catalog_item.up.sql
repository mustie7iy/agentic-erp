-- Validates 000277's foreign key in a transaction of its own (T84b): the
-- scan takes no lock that blocks reads or writes of requisition items.
ALTER TABLE procurement.requisition_item VALIDATE CONSTRAINT requisition_item_catalog_item_fkey;
