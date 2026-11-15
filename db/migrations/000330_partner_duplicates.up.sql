-- Duplicate check when a business partner is created (T89a; 03-data-model
-- §3.6): the partner change lists the existing partners with the same tax
-- registration number, or the same normalized name and country, as
-- possible duplicates for its approver. Expand only (06 §6.4): a function,
-- an index and a column with a default, which the previous version
-- neither reads nor writes; a change it writes lists none.

-- A partner's name as duplicates are matched by it: white space trimmed
-- and each run of it one space, in capitals. It is partner.NormalizedName,
-- which the migration's quality gate uses too, in SQL: the class is Go's
-- unicode.IsSpace, and both upper-case by Unicode (TestNormalizedName
-- holds them equal), so the index finds what the Go key names.
CREATE FUNCTION partner.normalized_name(name text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT
RETURN upper(btrim(regexp_replace(name, '[\s\u0085   -     　]+', ' ', 'g'), ' '));

-- The one index of the name match, by country as the match is.
CREATE INDEX business_partner_normalized_name ON partner.business_partner (tenant_id, country_code, partner.normalized_name(name));

-- What a create change found when it was requested: each existing partner
-- by id, number and name with what matched (tax_registration_number,
-- name_and_country), an array, empty when none matched.
ALTER TABLE partner.partner_change
  ADD COLUMN possible_duplicates jsonb NOT NULL DEFAULT '[]'
    CONSTRAINT partner_change_possible_duplicates_array CHECK (jsonb_typeof(possible_duplicates) = 'array');
