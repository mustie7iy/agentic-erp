-- A credit memo request (T85c; ADR-0067, As built T85c; 03-data-model
-- §3.29): what a sales administrator asks to be credited of a posted
-- billing document's items, either for the quantities a returns order of
-- that document received, at the billed amount of those quantities, or
-- as an amount per item with a reason. A sales manager approves or
-- rejects it, once. It is a document of the sales context alone and
-- posts nothing; posting an approved request as a customer credit memo
-- is T85d. Per billing item, the requests that are not rejected never
-- ask for more than the item billed, and per returns order item never
-- for more than was received. Nothing of the sales order, the billing
-- document or the returns order changes: none of them shows a request.
-- Expand only (06 §6.4): two tables and three permissions, which the
-- previous version neither reads nor writes.
CREATE TABLE sales.credit_memo_request (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL,
  document_number      text NOT NULL CHECK (length(document_number) BETWEEN 1 AND 40),
  legal_entity_id      uuid NOT NULL,       -- tenancy context
  customer_id          uuid NOT NULL,       -- partner context
  billing_document_id  uuid NOT NULL,
  sales_order_id       uuid NOT NULL,
  returns_order_id     uuid,                -- the returns order whose received quantities it credits; null for a request by amount
  request_date         date NOT NULL,
  reason               text CONSTRAINT credit_memo_request_reason_check CHECK (btrim(reason) <> '' AND length(reason) <= 500),
  status               text NOT NULL CONSTRAINT credit_memo_request_status_check CHECK (status IN ('open', 'approved', 'rejected')),
  rejection_reason     text CONSTRAINT credit_memo_request_rejection_reason_check CHECK (btrim(rejection_reason) <> '' AND length(rejection_reason) <= 500),
  version              integer NOT NULL DEFAULT 1,
  created_at           timestamptz NOT NULL DEFAULT now(),
  created_by           uuid NOT NULL,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT credit_memo_request_number_once UNIQUE (tenant_id, legal_entity_id, document_number),
  -- What a request's items reference, so an item is of its request's billing document.
  CONSTRAINT credit_memo_request_of_document UNIQUE (tenant_id, id, billing_document_id),
  -- The billing document with its sales order (billing_document_of_order, 000293).
  CONSTRAINT credit_memo_request_billing_document_fkey FOREIGN KEY (tenant_id, billing_document_id, sales_order_id)
    REFERENCES sales.billing_document (tenant_id, id, sales_order_id),
  -- A returns order of that billing document (returns_order_of_document, 000306).
  CONSTRAINT credit_memo_request_returns_order_fkey FOREIGN KEY (tenant_id, returns_order_id, billing_document_id)
    REFERENCES sales.returns_order (tenant_id, id, billing_document_id),
  -- A request by amount says why; one for a returns order has that order's reason.
  CONSTRAINT credit_memo_request_basis CHECK (returns_order_id IS NOT NULL OR reason IS NOT NULL),
  -- A rejected request says why, and only a rejected one does.
  CONSTRAINT credit_memo_request_rejection CHECK ((status = 'rejected') = (rejection_reason IS NOT NULL))
);
CREATE TRIGGER credit_memo_request_touch BEFORE UPDATE ON sales.credit_memo_request FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The list: a company's requests by date, a customer's, a billing document's, a sales order's and a returns order's.
CREATE INDEX credit_memo_request_by_date ON sales.credit_memo_request (tenant_id, legal_entity_id, request_date, id);
CREATE INDEX credit_memo_request_by_customer ON sales.credit_memo_request (tenant_id, customer_id, request_date, id);
CREATE INDEX credit_memo_request_by_document ON sales.credit_memo_request (tenant_id, billing_document_id);
CREATE INDEX credit_memo_request_by_order ON sales.credit_memo_request (tenant_id, sales_order_id);
CREATE INDEX credit_memo_request_by_returns_order ON sales.credit_memo_request (tenant_id, returns_order_id) WHERE returns_order_id IS NOT NULL;
SELECT platform.secure_table('sales.credit_memo_request'::regclass);

-- A request is created open, for a posted billing document of its
-- company and customer. The function only reads, so it runs as the
-- caller; row-level security applies.
CREATE FUNCTION sales.check_credit_memo_request() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status <> 'open' THEN
    RAISE EXCEPTION 'credit memo request %: it is created open', NEW.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'credit_memo_request_created_open';
  END IF;
  IF NOT EXISTS (SELECT FROM sales.billing_document b
      WHERE b.tenant_id = NEW.tenant_id AND b.id = NEW.billing_document_id AND b.status = 'posted'
        AND b.legal_entity_id = NEW.legal_entity_id AND b.customer_id = NEW.customer_id) THEN
    RAISE EXCEPTION 'credit memo request %: its billing document is not a posted one of its company and customer', NEW.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'credit_memo_request_document_posted';
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION sales.check_credit_memo_request() OWNER TO erp_owner;
CREATE TRIGGER credit_memo_request_document_posted AFTER INSERT ON sales.credit_memo_request
  FOR EACH ROW EXECUTE FUNCTION sales.check_credit_memo_request();

-- The decision is the only change: an open request is approved or
-- rejected, once, and what it asks for stays (the grant names the
-- columns the decision sets; nothing is deleted).
CREATE FUNCTION sales.check_credit_memo_request_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'open' OR NEW.status NOT IN ('approved', 'rejected') THEN
    RAISE EXCEPTION 'credit memo request %: only an open one changes, and only by being approved or rejected', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'credit_memo_request_decided_once';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION sales.check_credit_memo_request_change() OWNER TO erp_owner;
CREATE TRIGGER credit_memo_request_decided_once BEFORE UPDATE ON sales.credit_memo_request
  FOR EACH ROW EXECUTE FUNCTION sales.check_credit_memo_request_change();
REVOKE UPDATE, DELETE ON sales.credit_memo_request FROM erp_app;
GRANT UPDATE (status, rejection_reason, version, updated_by) ON sales.credit_memo_request TO erp_app;

-- An item of a request: an amount more than zero, in the billing
-- document's currency, credited of one item of the request's billing
-- document, a billing item at most once per request. An item of a
-- request for a returns order also names that returns order, whose item
-- of the same billing item it credits, and the quantity credited; an
-- item of a request by amount has neither. An item is written once and
-- never changed or deleted.
CREATE TABLE sales.credit_memo_request_item (
  tenant_id            uuid NOT NULL,
  request_id           uuid NOT NULL,
  item_no              integer NOT NULL CHECK (item_no > 0),
  billing_document_id  uuid NOT NULL,
  billing_item_no      integer NOT NULL,
  returns_order_id     uuid,
  quantity             numeric(20,6),
  net_amount           bigint NOT NULL CONSTRAINT credit_memo_request_item_amount_positive CHECK (net_amount > 0),
  PRIMARY KEY (tenant_id, request_id, item_no),
  CONSTRAINT credit_memo_request_item_once UNIQUE (tenant_id, request_id, billing_item_no),
  CONSTRAINT credit_memo_request_item_quantity CHECK ((returns_order_id IS NULL) = (quantity IS NULL) AND quantity > 0),
  CONSTRAINT credit_memo_request_item_request_fkey FOREIGN KEY (tenant_id, request_id, billing_document_id)
    REFERENCES sales.credit_memo_request (tenant_id, id, billing_document_id),
  CONSTRAINT credit_memo_request_item_billing_item_fkey FOREIGN KEY (tenant_id, billing_document_id, billing_item_no)
    REFERENCES sales.billing_document_item (tenant_id, billing_document_id, item_no),
  -- The returns order's item of that billing item (returns_order_item_once, 000306).
  CONSTRAINT credit_memo_request_item_returns_item_fkey FOREIGN KEY (tenant_id, returns_order_id, billing_item_no)
    REFERENCES sales.returns_order_item (tenant_id, returns_order_id, billing_item_no)
);
-- What the requests of a billing document ask for of one of its items, and of a returns order's item.
CREATE INDEX credit_memo_request_item_by_billing_item ON sales.credit_memo_request_item (tenant_id, billing_document_id, billing_item_no);
CREATE INDEX credit_memo_request_item_by_returns_item ON sales.credit_memo_request_item (tenant_id, returns_order_id, billing_item_no)
  WHERE returns_order_id IS NOT NULL;
CREATE TRIGGER credit_memo_request_item_immutable BEFORE UPDATE OR DELETE ON sales.credit_memo_request_item
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('sales.credit_memo_request_item'::regclass);

-- An item names the returns order its request is for, or none as its
-- request does. The requests of a billing document that are not
-- rejected never ask for more of one of its items than it billed, nor
-- for more of a returns order's item than was received. The application
-- reads the sales order FOR UPDATE first, which serializes the requests
-- of one order's billing documents (ADR-0008); a rejection only frees
-- what its request asked for. The function only reads, so it runs as the
-- caller; row-level security applies.
CREATE FUNCTION sales.check_credit_memo_request_item() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE of_returns uuid; billed bigint; asked bigint; received numeric; taken numeric;
BEGIN
  SELECT r.returns_order_id INTO of_returns FROM sales.credit_memo_request r WHERE r.tenant_id = NEW.tenant_id AND r.id = NEW.request_id;
  IF of_returns IS DISTINCT FROM NEW.returns_order_id THEN
    RAISE EXCEPTION 'credit memo request %: item % does not name the returns order its request is for', NEW.request_id, NEW.item_no
      USING ERRCODE = 'check_violation', CONSTRAINT = 'credit_memo_request_item_of_returns_order';
  END IF;
  SELECT b.net_amount INTO billed FROM sales.billing_document_item b
  WHERE b.tenant_id = NEW.tenant_id AND b.billing_document_id = NEW.billing_document_id AND b.item_no = NEW.billing_item_no;
  SELECT sum(i.net_amount), sum(i.quantity) FILTER (WHERE i.returns_order_id = NEW.returns_order_id) INTO asked, taken
  FROM sales.credit_memo_request_item i JOIN sales.credit_memo_request r ON r.tenant_id = i.tenant_id AND r.id = i.request_id
  WHERE i.tenant_id = NEW.tenant_id AND i.billing_document_id = NEW.billing_document_id AND i.billing_item_no = NEW.billing_item_no
    AND r.status <> 'rejected';
  IF asked > billed THEN
    RAISE EXCEPTION 'credit memo request %: item % of its billing document would have % credited of % billed', NEW.request_id, NEW.billing_item_no, asked, billed
      USING ERRCODE = 'check_violation', CONSTRAINT = 'credit_memo_request_item_amounts';
  END IF;
  IF NEW.returns_order_id IS NOT NULL THEN
    SELECT o.received_quantity INTO received FROM sales.returns_order_item o
    WHERE o.tenant_id = NEW.tenant_id AND o.returns_order_id = NEW.returns_order_id AND o.billing_item_no = NEW.billing_item_no;
    IF taken > received THEN
      RAISE EXCEPTION 'credit memo request %: item % of its returns order would have % credited of % received', NEW.request_id, NEW.billing_item_no, taken, received
        USING ERRCODE = 'check_violation', CONSTRAINT = 'credit_memo_request_item_quantities';
    END IF;
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION sales.check_credit_memo_request_item() OWNER TO erp_owner;
CREATE TRIGGER credit_memo_request_item_amounts AFTER INSERT ON sales.credit_memo_request_item
  FOR EACH ROW EXECUTE FUNCTION sales.check_credit_memo_request_item();

-- Reading credit memo requests, creating them and deciding them
-- (ADR-0067 §7). Approving is by this one permission, without an amount
-- tier, and nothing sets the approver apart from the creator (product
-- owner 2026-11-12); as the sales permissions before it, it is no
-- approval permission of the workflow context.
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('sales.credit_memo_request.read', 'View credit memo requests of the companies one may act in', false),
  ('sales.credit_memo_request.write', 'Create credit memo requests against posted billing documents', false),
  ('sales.credit_memo_request.approve', 'Approve or reject credit memo requests', false);
