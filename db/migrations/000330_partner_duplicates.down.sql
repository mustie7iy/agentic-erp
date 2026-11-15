ALTER TABLE partner.partner_change DROP COLUMN possible_duplicates;
DROP INDEX partner.business_partner_normalized_name;
DROP FUNCTION partner.normalized_name(text);
