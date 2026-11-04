CREATE OR REPLACE FUNCTION assets.check_asset_assignment() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE current integer; first boolean;
BEGIN
  SELECT a.last_seq INTO current FROM assets.asset a WHERE a.tenant_id = NEW.tenant_id AND a.id = NEW.asset_id;
  SELECT NOT EXISTS (SELECT FROM assets.asset_assignment r WHERE r.tenant_id = NEW.tenant_id AND r.asset_id = NEW.asset_id) INTO first;
  IF NEW.seq IS DISTINCT FROM current THEN
    RAISE EXCEPTION 'asset %: an assignment takes the asset''s last seq %, not %', NEW.asset_id, current, NEW.seq
      USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_assignment_seq';
  END IF;
  IF first <> (NEW.valid_from = '-infinity'::date) THEN
    RAISE EXCEPTION 'asset %: only its first assignment is valid from -infinity', NEW.asset_id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'asset_assignment_valid_from';
  END IF;
  RETURN NEW;
END $$;
ALTER TABLE assets.asset_assignment DROP COLUMN restores_seq;
DROP TABLE assets.transfer;
DROP FUNCTION assets.check_transfer();
