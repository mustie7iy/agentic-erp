-- Material requirements planning (S2k; ADR-0084; 03-data-model §3.37):
-- the mrp context's schema. A material's planning data at a plant (SAP
-- MARC's MRP view): how it is planned and its reorder point; a planning
-- run per plant (SAP MD01/MRP Live for one plant), queued with its
-- Operation and carried out as a workflow; the run's line per material
-- planned, with what it counted and the shortage it found; and the
-- purchase requisitions runs proposed, so that a later run replaces its
-- own open proposals and never a person's requisition. Materials,
-- plants, legal entities, accounts and requisitions are the materials,
-- tenancy, ledger and procurement contexts', by id: no foreign key
-- leaves the schema but ref's (02 §2.1).
--
-- Expand only (06 §6.4): a new schema, new tables and three permissions.
CREATE SCHEMA mrp;
GRANT USAGE ON SCHEMA mrp TO erp_app, erp_readonly, erp_owner;

-- A material's planning data at a plant: reorder_point planning (SAP's
-- MRP type VB, from memory) proposes what brings the plant's available
-- quantity back to the reorder point; no_planning (ND) plans nothing. The
-- reorder point is in the material's base unit. account_id is the
-- account its proposed requisitions name (procurement requires one; a
-- stock item's receipt posts its valuation class's accounts, ADR-0082 As
-- built S1e).
CREATE TABLE mrp.material_plan (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  legal_entity_id  uuid NOT NULL,    -- tenancy context: the plant's company
  plant_id         uuid NOT NULL,    -- tenancy context: an org unit of kind plant
  material_id      uuid NOT NULL,    -- materials context
  mrp_type         text NOT NULL CONSTRAINT material_plan_mrp_type_check CHECK (mrp_type IN ('reorder_point', 'no_planning')),
  reorder_point    numeric(20,6) NOT NULL CONSTRAINT material_plan_reorder_point_check CHECK (reorder_point >= 0),
  account_id       uuid NOT NULL,    -- ledger context
  version          integer NOT NULL DEFAULT 1 CONSTRAINT material_plan_version_check CHECK (version >= 1),
  created_at       timestamptz NOT NULL DEFAULT now(),
  created_by       uuid NOT NULL,
  updated_at       timestamptz NOT NULL DEFAULT now(),
  updated_by       uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT material_plan_once UNIQUE (tenant_id, plant_id, material_id)
);
CREATE TRIGGER material_plan_touch BEFORE UPDATE ON mrp.material_plan FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- A plan's company, plant and material do not change.
CREATE FUNCTION mrp.material_plan_fixed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.legal_entity_id <> OLD.legal_entity_id OR NEW.plant_id <> OLD.plant_id OR NEW.material_id <> OLD.material_id THEN
    RAISE EXCEPTION 'a material plan''s company, plant and material do not change' USING ERRCODE = 'check_violation', CONSTRAINT = 'material_plan_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION mrp.material_plan_fixed() OWNER TO erp_owner;
CREATE TRIGGER material_plan_fixed BEFORE UPDATE ON mrp.material_plan FOR EACH ROW EXECUTE FUNCTION mrp.material_plan_fixed();
-- The list: the tenant's plans in the order they were created, in the order its pages are read.
CREATE INDEX material_plan_by_created_at ON mrp.material_plan (tenant_id, created_at, id);
SELECT platform.secure_table('mrp.material_plan'::regclass);

-- A planning run of one plant: queued with its Operation, then completed
-- on its company's day (run_date) with what it planned, or failed with
-- the problem that stopped it. One run of a plant is queued at a time.
-- Once it ended it changes no more.
CREATE TABLE mrp.run (
  tenant_id           uuid NOT NULL,
  id                  uuid NOT NULL,
  legal_entity_id     uuid NOT NULL,    -- tenancy context: the plant's company
  plant_id            uuid NOT NULL,    -- tenancy context
  operation_id        uuid NOT NULL,    -- operations context: the run's Operation
  status              text NOT NULL DEFAULT 'queued' CONSTRAINT run_status_check CHECK (status IN ('queued', 'completed', 'failed')),
  run_date            date,
  materials_planned   integer CONSTRAINT run_materials_planned_check CHECK (materials_planned >= 0),
  proposals_created   integer CONSTRAINT run_proposals_created_check CHECK (proposals_created >= 0),
  proposals_replaced  integer CONSTRAINT run_proposals_replaced_check CHECK (proposals_replaced >= 0),
  error               jsonb,
  version             integer NOT NULL DEFAULT 1 CONSTRAINT run_version_check CHECK (version >= 1),
  created_at          timestamptz NOT NULL DEFAULT now(),
  created_by          uuid NOT NULL,
  updated_at          timestamptz NOT NULL DEFAULT now(),
  updated_by          uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  -- A completed run names its day and counts; a failed one its problem; a queued one neither.
  CONSTRAINT run_outcome CHECK ((status = 'completed') = (run_date IS NOT NULL) AND (status = 'completed') = (materials_planned IS NOT NULL)
    AND (status = 'completed') = (proposals_created IS NOT NULL) AND (status = 'completed') = (proposals_replaced IS NOT NULL)
    AND (status = 'failed') = (error IS NOT NULL))
);
CREATE UNIQUE INDEX run_one_at_a_time ON mrp.run (tenant_id, plant_id) WHERE status = 'queued';
CREATE INDEX run_by_created_at ON mrp.run (tenant_id, created_at, id);
CREATE TRIGGER run_touch BEFORE UPDATE ON mrp.run FOR EACH ROW EXECUTE FUNCTION platform.touch();
CREATE FUNCTION mrp.run_open() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status <> 'queued' THEN
    RAISE EXCEPTION 'planning run %: it is %, so it changes no more', OLD.id, OLD.status USING ERRCODE = 'check_violation', CONSTRAINT = 'run_ended_fixed';
  END IF;
  IF NEW.legal_entity_id <> OLD.legal_entity_id OR NEW.plant_id <> OLD.plant_id OR NEW.operation_id <> OLD.operation_id THEN
    RAISE EXCEPTION 'planning run %: its company, plant and operation do not change', OLD.id USING ERRCODE = 'check_violation', CONSTRAINT = 'run_ended_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION mrp.run_open() OWNER TO erp_owner;
CREATE TRIGGER run_ended_fixed BEFORE UPDATE ON mrp.run FOR EACH ROW EXECUTE FUNCTION mrp.run_open();
SELECT platform.secure_table('mrp.run'::regclass);

-- A completed run's line per material it planned, in the material's base
-- unit: the reorder point, the open sales order demand, the plant's
-- stock and its open receipts as the run counted them, and the shortage
-- (what brings the available quantity back to the reorder point), with
-- the requisition proposed for it: one exactly when there is a shortage.
-- Insert-only.
CREATE TABLE mrp.run_line (
  tenant_id           uuid NOT NULL,
  run_id              uuid NOT NULL,
  material_id         uuid NOT NULL,    -- materials context
  uom_code            text NOT NULL CONSTRAINT run_line_uom_fkey REFERENCES ref.unit_of_measure,
  reorder_point       numeric(20,6) NOT NULL CONSTRAINT run_line_reorder_point_check CHECK (reorder_point >= 0),
  demand              numeric(20,6) NOT NULL CONSTRAINT run_line_demand_check CHECK (demand >= 0),
  stock               numeric(20,6) NOT NULL CONSTRAINT run_line_stock_check CHECK (stock >= 0),
  receipts            numeric(20,6) NOT NULL CONSTRAINT run_line_receipts_check CHECK (receipts >= 0),
  shortage            numeric(20,6) NOT NULL CONSTRAINT run_line_shortage_check CHECK (shortage >= 0),
  requisition_id      uuid,             -- procurement context
  requisition_number  text CONSTRAINT run_line_requisition_number_check CHECK (length(requisition_number) BETWEEN 1 AND 40),
  PRIMARY KEY (tenant_id, run_id, material_id),
  CONSTRAINT run_line_run_fkey FOREIGN KEY (tenant_id, run_id) REFERENCES mrp.run (tenant_id, id),
  CONSTRAINT run_line_proposed CHECK ((shortage > 0) = (requisition_id IS NOT NULL) AND (requisition_id IS NULL) = (requisition_number IS NULL))
);
CREATE TRIGGER run_line_immutable BEFORE UPDATE OR DELETE ON mrp.run_line FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('mrp.run_line'::regclass);

-- A purchase requisition a run proposed, at the version it was created
-- in: open while it is the run's own (a draft no person changed), which
-- the plant's next run replaces (cancels); taken_over once a person
-- changed or submitted it, which the next runs count as an open receipt
-- and never touch; closed once it is ordered, cancelled or rejected,
-- counted no more. replaced and closed are final.
CREATE TABLE mrp.proposal (
  tenant_id              uuid NOT NULL,
  requisition_id         uuid NOT NULL,    -- procurement context
  requisition_number     text NOT NULL CONSTRAINT proposal_requisition_number_check CHECK (length(requisition_number) BETWEEN 1 AND 40),
  requisition_version    integer NOT NULL CONSTRAINT proposal_requisition_version_check CHECK (requisition_version >= 1),
  run_id                 uuid NOT NULL,
  plant_id               uuid NOT NULL,    -- tenancy context
  material_id            uuid NOT NULL,    -- materials context
  status                 text NOT NULL DEFAULT 'open' CONSTRAINT proposal_status_check CHECK (status IN ('open', 'replaced', 'taken_over', 'closed')),
  replaced_by_run_id     uuid,
  PRIMARY KEY (tenant_id, requisition_id),
  CONSTRAINT proposal_run_fkey FOREIGN KEY (tenant_id, run_id) REFERENCES mrp.run (tenant_id, id),
  CONSTRAINT proposal_replaced_by_fkey FOREIGN KEY (tenant_id, replaced_by_run_id) REFERENCES mrp.run (tenant_id, id),
  CONSTRAINT proposal_replaced CHECK ((status = 'replaced') = (replaced_by_run_id IS NOT NULL))
);
-- A plant's proposals still counted or replaceable, for its next run.
CREATE INDEX proposal_by_plant ON mrp.proposal (tenant_id, plant_id) WHERE status IN ('open', 'taken_over');
CREATE FUNCTION mrp.proposal_moves() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status IN ('replaced', 'closed') OR (OLD.status = 'taken_over' AND NEW.status NOT IN ('taken_over', 'closed'))
     OR NEW.requisition_id <> OLD.requisition_id OR NEW.requisition_number <> OLD.requisition_number
     OR NEW.requisition_version <> OLD.requisition_version OR NEW.run_id <> OLD.run_id OR NEW.plant_id <> OLD.plant_id
     OR NEW.material_id <> OLD.material_id THEN
    RAISE EXCEPTION 'proposal %: it moves only from open, or from taken_over to closed', OLD.requisition_number
      USING ERRCODE = 'check_violation', CONSTRAINT = 'proposal_moves';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION mrp.proposal_moves() OWNER TO erp_owner;
CREATE TRIGGER proposal_moves BEFORE UPDATE ON mrp.proposal FOR EACH ROW EXECUTE FUNCTION mrp.proposal_moves();
CREATE TRIGGER proposal_no_delete BEFORE DELETE ON mrp.proposal FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('mrp.proposal'::regclass);
GRANT UPDATE (status, replaced_by_run_id) ON mrp.proposal TO erp_app;

-- Reading and maintaining materials' planning data and runs, and
-- starting runs (ADR-0084 §5).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('mrp.plan.read', 'View materials'' planning data and planning runs', false),
  ('mrp.plan.write', 'Maintain materials'' planning data', false),
  ('mrp.run.start', 'Start planning runs, which propose purchase requisitions', false);
