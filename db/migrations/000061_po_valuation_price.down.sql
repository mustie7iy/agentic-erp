DROP TRIGGER purchase_order_item_valued ON procurement.purchase_order_item;
DROP TRIGGER purchase_order_items_valued ON procurement.purchase_order;
DROP TRIGGER purchase_order_item_valuation_fixed ON procurement.purchase_order_item;
DROP FUNCTION procurement.check_item_valued();
DROP FUNCTION procurement.check_order_items_valued();
DROP FUNCTION procurement.check_valuation_price_fixed();
ALTER TABLE procurement.purchase_order_item DROP CONSTRAINT purchase_order_item_valuation_pair,
  DROP COLUMN valuation_price_unit, DROP COLUMN valuation_price;
