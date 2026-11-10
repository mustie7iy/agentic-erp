-- The order a payment run tries a company's payment methods in (ADR-0038
-- amendment; SAP specialist 2026-11-09, A4(c)): SAP's payment run takes
-- the order from its own parameters per company code and a supplier's
-- methods (LFB1-ZWELS) only restrict it, where a proposal took the first
-- method in the supplier's own list. run_order is the method's place in
-- the company's order, 1 first, written from the configuration's
-- companies[].payment_methods by payments.PaymentMethods on every
-- activation. It stays nullable: a replica of the release before writes
-- none, and a method without one is tried after those with one, by code.
-- The check is added NOT VALID and validated by 000248 (06 §6.4).
ALTER TABLE payments.payment_method ADD COLUMN run_order integer;
ALTER TABLE payments.payment_method ADD CONSTRAINT payment_method_run_order CHECK (run_order > 0) NOT VALID;

-- The methods of existing tenants take the order of their company's
-- payment_methods in the active configuration, which is the order every
-- later activation writes for a document that gives its methods none of
-- their own, so a proposal's choice does not move again at the next
-- activation. A method the active document no longer has (inactive) keeps
-- none. Tenant rows: under a role that does not bypass row-level security
-- this fails rather than order nothing, as 000042.
SET LOCAL lock_timeout = '5s';
SET LOCAL row_security = off;

UPDATE payments.payment_method m SET run_order = d.n, version = m.version + 1
FROM (SELECT a.tenant_id, le.id AS legal_entity_id, pm.method ->> 'code' AS code, pm.n
    FROM config.active_version a
    JOIN config.config_version v ON v.tenant_id = a.tenant_id AND v.number = a.number
    CROSS JOIN LATERAL jsonb_array_elements(v.document -> 'companies') AS co(company)
    JOIN tenancy.legal_entity le ON le.tenant_id = a.tenant_id AND le.code = co.company ->> 'code'
    CROSS JOIN LATERAL jsonb_array_elements(co.company -> 'payment_methods') WITH ORDINALITY AS pm(method, n)) d
WHERE m.tenant_id = d.tenant_id AND m.legal_entity_id = d.legal_entity_id AND m.code = d.code;
