DELETE FROM identity.sod_rule WHERE permission_a = 'inventory.count.write' OR permission_b = 'inventory.count.write';
DELETE FROM identity.role_permission WHERE permission_code = 'inventory.count.write';
DELETE FROM ref.permission WHERE code = 'inventory.count.write';
DROP TRIGGER count_posting_block ON inventory.movement;
DROP FUNCTION inventory.count_posting_block();
DROP TABLE inventory.count_item;
DROP FUNCTION inventory.count_item_once();
DROP TABLE inventory.count_document;
DROP FUNCTION inventory.count_document_status_flow();
