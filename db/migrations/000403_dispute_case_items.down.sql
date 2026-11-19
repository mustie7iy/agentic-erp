DROP TRIGGER dispute_case_default_item ON collections.dispute_case;
DROP TRIGGER dispute_case_items ON collections.dispute_case;
DROP TRIGGER dispute_case_items_closed ON collections.dispute_case;
DROP TABLE collections.dispute_case_item;
DROP FUNCTION collections.check_dispute_case_items();
DROP FUNCTION collections.default_dispute_case_item();
DROP FUNCTION collections.close_dispute_case_items();
DROP FUNCTION collections.check_dispute_case_item_change();
