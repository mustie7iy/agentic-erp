DELETE FROM identity.sod_rule WHERE permission_a IN ('controlling.allocation.read', 'controlling.allocation.write', 'controlling.allocation.run')
  OR permission_b IN ('controlling.allocation.read', 'controlling.allocation.write', 'controlling.allocation.run');
DELETE FROM identity.role_permission WHERE permission_code IN ('controlling.allocation.read', 'controlling.allocation.write', 'controlling.allocation.run');
DELETE FROM ref.permission WHERE code IN ('controlling.allocation.read', 'controlling.allocation.write', 'controlling.allocation.run');
DROP TABLE controlling.allocation_run;
DROP TABLE controlling.allocation_receiver;
DROP TABLE controlling.allocation_cycle;
DROP FUNCTION controlling.check_allocation_receiver_shares();
DROP FUNCTION controlling.check_allocation_cycle_shares();
DROP FUNCTION controlling.allocation_shares_whole(uuid, uuid);
DROP FUNCTION controlling.check_allocation_cycle();
