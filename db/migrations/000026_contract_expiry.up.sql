-- Contract expiry: a job moves active contracts past valid_to to expired
-- (ContractExpired). Finding the tenants with contracts due crosses
-- tenants, which the worker role does; the expiry itself runs in the
-- tenant's scope as erp_app, like an operator's transition.
GRANT USAGE ON SCHEMA procurement TO erp_worker;
GRANT SELECT (tenant_id, status, valid_to) ON procurement.contract TO erp_worker;
