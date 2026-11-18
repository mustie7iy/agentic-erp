-- Prices per customer price group (T80f; ADR-0067 §13; 03-data-model
-- §3.29): a customer's sales data in a company holds its price group, one
-- of the configuration's (pricing.price_groups), and a price record may
-- be for a price group instead of for one customer. An item takes the
-- customer's own record first, then its group's, then the general one.
-- The configuration is the configuration context's: a group has no
-- foreign key (02 §2.1), and it is never removed once activated.
-- legal_entity_id is the tenancy context's and customer_id the partner
-- context's. Expand only (06 §6.4): a new table and a column the previous
-- version neither reads nor writes, null on its rows.
CREATE TABLE sales.customer_sales_data (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,       -- tenancy context
  customer_id      uuid NOT NULL,       -- partner context
  customer_number  text NOT NULL CONSTRAINT customer_sales_data_customer_number_check CHECK (length(customer_number) BETWEEN 1 AND 40),
  price_group      text CONSTRAINT customer_sales_data_price_group_check CHECK (price_group ~ '^[A-Z0-9_]{1,10}$'),
  version          integer NOT NULL DEFAULT 1 CONSTRAINT customer_sales_data_version_check CHECK (version >= 1),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  -- One per customer and company; pricing finds it by them.
  CONSTRAINT customer_sales_data_once UNIQUE (tenant_id, legal_entity_id, customer_id)
);
CREATE TRIGGER customer_sales_data_touch BEFORE UPDATE ON sales.customer_sales_data FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list: the tenant's sales data by customer number, in the order its
-- pages are read.
CREATE INDEX customer_sales_data_by_customer_number ON sales.customer_sales_data (tenant_id, customer_number, id);
SELECT platform.secure_table('sales.customer_sales_data'::regclass);
-- A change sets the price group alone: the customer and the company stay,
-- and no sales data is deleted.
REVOKE UPDATE ON sales.customer_sales_data FROM erp_app;
GRANT UPDATE (price_group, version, updated_by) ON sales.customer_sales_data TO erp_app;

-- A record for a price group is for no one customer.
ALTER TABLE sales.price
  ADD COLUMN price_group text CONSTRAINT price_price_group_check CHECK (price_group ~ '^[A-Z0-9_]{1,10}$'),
  ADD CONSTRAINT price_customer_or_group CHECK (customer_id IS NULL OR price_group IS NULL);
