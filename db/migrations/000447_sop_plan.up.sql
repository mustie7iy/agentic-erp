-- Sales and operations planning (S2n; ADR-0084 As built S2n; 03-data-model
-- §3.37): a company's S&OP plan versions. A version reviews the demand
-- forecast against supply by material group, base unit and month on the
-- company's day, the planner sets its consensus while it is a draft, and
-- fixed it changes no more (SAP IBP's S&OP review and its consensus
-- version, from memory). Legal entities, materials and units are the
-- tenancy, materials and ref contexts', by id or code: no foreign key
-- leaves the schema but ref's (02 §2.1).
--
-- Expand only (06 §6.4): new tables and two permissions.

-- A plan version of a company, numbered from 1 per company: draft, then
-- fixed with who fixed it and when. Its horizon is the months calendar
-- months from the day after the day it reviewed on (as_of). Nothing about
-- it changes but its status (draft to fixed) and version, and a fixed one
-- not at all; none is deleted.
CREATE TABLE mrp.sop_plan (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,    -- tenancy context: the company
  number           integer NOT NULL CONSTRAINT sop_plan_number_check CHECK (number >= 1),
  status           text NOT NULL DEFAULT 'draft' CONSTRAINT sop_plan_status_check CHECK (status IN ('draft', 'fixed')),
  as_of            date NOT NULL,
  first_day        date NOT NULL,
  last_day         date NOT NULL,
  months           smallint NOT NULL CONSTRAINT sop_plan_months_check CHECK (months BETWEEN 1 AND 12),
  fixed_at         timestamptz,
  fixed_by         uuid,
  version          integer NOT NULL DEFAULT 1 CONSTRAINT sop_plan_version_check CHECK (version >= 1),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT sop_plan_number_once UNIQUE (tenant_id, legal_entity_id, number),
  -- The horizon starts the day after the review and ends on the last day
  -- of its last month.
  CONSTRAINT sop_plan_horizon CHECK (first_day = as_of + 1
    AND last_day = (date_trunc('month', first_day) + make_interval(months => months) - interval '1 day')::date),
  -- A fixed version names who fixed it and when; a draft neither.
  CONSTRAINT sop_plan_fixed_by CHECK ((status = 'fixed') = (fixed_at IS NOT NULL) AND (status = 'fixed') = (fixed_by IS NOT NULL))
);
CREATE INDEX sop_plan_by_created_at ON mrp.sop_plan (tenant_id, created_at, id);
CREATE TRIGGER sop_plan_touch BEFORE UPDATE ON mrp.sop_plan FOR EACH ROW EXECUTE FUNCTION platform.touch();
CREATE FUNCTION mrp.sop_plan_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'S&OP plan %: a plan version is never deleted', OLD.number USING ERRCODE = 'check_violation', CONSTRAINT = 'sop_plan_fixed';
  END IF;
  IF OLD.status = 'fixed' THEN
    RAISE EXCEPTION 'S&OP plan %: it is fixed, so it changes no more', OLD.number USING ERRCODE = 'check_violation', CONSTRAINT = 'sop_plan_fixed';
  END IF;
  IF (NEW.legal_entity_id, NEW.number, NEW.as_of, NEW.first_day, NEW.last_day, NEW.months, NEW.created_at, NEW.created_by)
     IS DISTINCT FROM (OLD.legal_entity_id, OLD.number, OLD.as_of, OLD.first_day, OLD.last_day, OLD.months, OLD.created_at, OLD.created_by) THEN
    RAISE EXCEPTION 'S&OP plan %: its company, number and horizon do not change', OLD.number USING ERRCODE = 'check_violation', CONSTRAINT = 'sop_plan_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION mrp.sop_plan_fixed() OWNER TO erp_owner;
CREATE TRIGGER sop_plan_fixed BEFORE UPDATE OR DELETE ON mrp.sop_plan FOR EACH ROW EXECUTE FUNCTION mrp.sop_plan_fixed();
SELECT platform.secure_table('mrp.sop_plan'::regclass);

-- A version's line per material group, base unit and month (month, its
-- first day; first_day..last_day the days of it in the horizon), in that
-- unit: the forecast (per material and plant the greater of its moving
-- average of goods issues over the days and its orders due: confirmed
-- sales orders and intercompany orders it supplies, so never below the
-- orders), those orders, and in the first month the plants' stock,
-- purchase orders' open quantities, stock transport orders in transit to
-- them and the proposed requisitions; the consensus the planner agrees. A
-- line is written with its version, a draft; only its consensus changes,
-- and only while the version is a draft; no line is deleted.
CREATE TABLE mrp.sop_plan_line (
  tenant_id         uuid NOT NULL,
  plan_id           uuid NOT NULL,
  material_group    text NOT NULL CONSTRAINT sop_plan_line_material_group_check CHECK (material_group ~ '^[A-Z0-9_-]{1,9}$'),
  uom_code          text NOT NULL CONSTRAINT sop_plan_line_uom_fkey REFERENCES ref.unit_of_measure,
  month             date NOT NULL,
  first_day         date NOT NULL,
  last_day          date NOT NULL,
  materials         integer NOT NULL CONSTRAINT sop_plan_line_materials_check CHECK (materials >= 1),
  forecast          numeric(20,6) NOT NULL CONSTRAINT sop_plan_line_forecast_check CHECK (forecast >= 0),
  orders            numeric(20,6) NOT NULL CONSTRAINT sop_plan_line_orders_check CHECK (orders >= 0),
  stock             numeric(20,6) NOT NULL CONSTRAINT sop_plan_line_stock_check CHECK (stock >= 0),
  purchase_orders   numeric(20,6) NOT NULL CONSTRAINT sop_plan_line_purchase_orders_check CHECK (purchase_orders >= 0),
  stock_transports  numeric(20,6) NOT NULL CONSTRAINT sop_plan_line_stock_transports_check CHECK (stock_transports >= 0),
  proposals         numeric(20,6) NOT NULL CONSTRAINT sop_plan_line_proposals_check CHECK (proposals >= 0),
  consensus         numeric(20,6) NOT NULL CONSTRAINT sop_plan_line_consensus_check CHECK (consensus >= 0),
  PRIMARY KEY (tenant_id, plan_id, material_group, uom_code, month),
  CONSTRAINT sop_plan_line_plan_fkey FOREIGN KEY (tenant_id, plan_id) REFERENCES mrp.sop_plan (tenant_id, id),
  -- The days are of the month, the first on or after its first day.
  CONSTRAINT sop_plan_line_days CHECK (month = date_trunc('month', month)::date AND first_day >= month AND first_day <= last_day
    AND last_day = (month + interval '1 month' - interval '1 day')::date),
  -- The orders consume the forecast, which is never below them.
  CONSTRAINT sop_plan_line_forecast_covers_orders CHECK (forecast >= orders)
);
CREATE FUNCTION mrp.sop_plan_line_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE st text;
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'S&OP plan line %: a line is never deleted', OLD.material_group USING ERRCODE = 'check_violation', CONSTRAINT = 'sop_plan_line_fixed';
  END IF;
  IF TG_OP = 'UPDATE' AND (to_jsonb(NEW) - 'consensus') IS DISTINCT FROM (to_jsonb(OLD) - 'consensus') THEN
    RAISE EXCEPTION 'S&OP plan line %: only its consensus changes', OLD.material_group USING ERRCODE = 'check_violation', CONSTRAINT = 'sop_plan_line_fixed';
  END IF;
  SELECT status INTO st FROM mrp.sop_plan WHERE tenant_id = NEW.tenant_id AND id = NEW.plan_id;
  IF st IS DISTINCT FROM 'draft' THEN
    RAISE EXCEPTION 'S&OP plan line %: its plan version is %, so it changes no more', NEW.material_group, coalesce(st, 'not readable')
      USING ERRCODE = 'check_violation', CONSTRAINT = 'sop_plan_line_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION mrp.sop_plan_line_fixed() OWNER TO erp_owner;
CREATE TRIGGER sop_plan_line_fixed BEFORE INSERT OR UPDATE OR DELETE ON mrp.sop_plan_line FOR EACH ROW EXECUTE FUNCTION mrp.sop_plan_line_fixed();
SELECT platform.secure_table('mrp.sop_plan_line'::regclass);
GRANT UPDATE (consensus) ON mrp.sop_plan_line TO erp_app;

-- Reading S&OP plan versions; starting them, setting their consensus and
-- fixing them (ADR-0084 As built S2n).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('mrp.sop.read', 'View S&OP plan versions', false),
  ('mrp.sop.write', 'Start S&OP reviews, set their consensus and fix them', false);
