DELETE FROM identity.sod_rule WHERE permission_a = 'cash.variance.comment' OR permission_b = 'cash.variance.comment';
DELETE FROM identity.role_permission WHERE permission_code = 'cash.variance.comment';
DELETE FROM ref.permission WHERE code = 'cash.variance.comment';
-- Retention as 000192 defined it.
CREATE OR REPLACE FUNCTION cash.delete_expired_snapshots(entity uuid, on_day date)
RETURNS TABLE (deleted_id uuid, deleted_kind text, deleted_date date)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  IF on_day > current_date + 1 THEN
    RAISE EXCEPTION 'snapshot retention on %: a day after tomorrow', on_day
      USING ERRCODE = 'check_violation', CONSTRAINT = 'snapshot_retention_day';
  END IF;
  RETURN QUERY DELETE FROM cash.snapshot s
    WHERE s.legal_entity_id = entity AND s.snapshot_date < (on_day - interval '25 months')::date
    RETURNING s.id, s.kind, s.snapshot_date;
END $$;
DROP TABLE cash.variance_comment;
DROP INDEX cash.snapshot_company;
