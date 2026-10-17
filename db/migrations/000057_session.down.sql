REVOKE ALL ON identity.session FROM erp_worker;
REVOKE USAGE ON SCHEMA identity FROM erp_worker;
DROP TABLE identity.session;
DROP FUNCTION identity.check_session_update();
