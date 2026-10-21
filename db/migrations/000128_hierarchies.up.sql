-- Standard and alternative hierarchies of cost and profit centers (T56b;
-- ADR-0050 §3, §11; 03-data-model §3.7). A hierarchy is a tree of nodes
-- (SAP's cost and profit center groups, set classes 0101 and 0106) with
-- centers of its kind at its leaves. Each tenant has at most one standard
-- hierarchy per kind, and every active center sits in exactly one leaf of
-- it; alternative hierarchies group centers for reporting, each center at
-- most once per hierarchy. Hierarchies are not time-dependent (decision
-- P7): reports show history under today's tree, as SAP does.
--
-- Nothing is deleted (000011): a node or a center's place in an
-- alternative hierarchy is removed by the change that removes it
-- (removed_by_change_id), and the tree is what is not removed. Every
-- change to a tree is a center_change a controller approves, planned when
-- requested and planned again when applied (ADR-0050 §11).
--
-- Expand only: center_change gains a hierarchy and kinds; nothing T56a
-- wrote changes meaning.

CREATE TABLE ledger.hierarchy (
  tenant_id    uuid NOT NULL,
  id           uuid NOT NULL,
  kind         text NOT NULL CHECK (kind IN ('cost_center', 'profit_center')),
  code         text NOT NULL CHECK (code ~ '^[0-9A-Z_-]{1,15}$'),
  name         text NOT NULL CHECK (name <> ''),
  is_standard  boolean NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  created_by   uuid NOT NULL,
  updated_at   timestamptz NOT NULL DEFAULT now(),
  updated_by   uuid NOT NULL,
  version      integer NOT NULL DEFAULT 1,  -- moves on with every change to the tree: the tree's ETag
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT hierarchy_code_once UNIQUE (tenant_id, kind, code),
  -- Members name the hierarchy with its kind, so a cost center cannot sit
  -- in a profit center hierarchy.
  CONSTRAINT hierarchy_of_kind UNIQUE (tenant_id, id, kind)
);
CREATE UNIQUE INDEX hierarchy_one_standard ON ledger.hierarchy (tenant_id, kind) WHERE is_standard;
CREATE TRIGGER hierarchy_touch BEFORE UPDATE ON ledger.hierarchy FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.hierarchy'::regclass);

-- A hierarchy's kind, code and standing are fixed.
CREATE FUNCTION ledger.check_hierarchy_fixed() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.kind, NEW.code, NEW.is_standard) IS DISTINCT FROM (OLD.kind, OLD.code, OLD.is_standard) THEN
    RAISE EXCEPTION 'hierarchy: the kind, code and standing of a hierarchy are fixed' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_hierarchy_fixed() OWNER TO erp_owner;
CREATE TRIGGER hierarchy_fixed BEFORE UPDATE ON ledger.hierarchy FOR EACH ROW EXECUTE FUNCTION ledger.check_hierarchy_fixed();

-- valid trees: one root (no parent), created with the hierarchy and never
-- moved or removed; a parent in the same hierarchy (a foreign key) and not
-- removed; no cycles and at most 12 levels (check_hierarchy_node).
CREATE TABLE ledger.hierarchy_node (
  tenant_id             uuid NOT NULL,
  id                    uuid NOT NULL,
  hierarchy_id          uuid NOT NULL,
  parent_id             uuid,
  code                  text NOT NULL CHECK (code ~ '^[0-9A-Z_-]{1,15}$'),
  name                  text NOT NULL CHECK (name <> ''),
  position              integer NOT NULL DEFAULT 0 CHECK (position >= 0),  -- order among its siblings
  change_id             uuid,               -- the change that created it; NULL for a root
  removed_by_change_id  uuid,
  created_at            timestamptz NOT NULL DEFAULT now(),
  created_by            uuid NOT NULL,
  updated_at            timestamptz NOT NULL DEFAULT now(),
  updated_by            uuid NOT NULL,
  version               integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT hierarchy_node_of UNIQUE (tenant_id, hierarchy_id, id),
  FOREIGN KEY (tenant_id, hierarchy_id) REFERENCES ledger.hierarchy (tenant_id, id),
  CONSTRAINT hierarchy_node_parent FOREIGN KEY (tenant_id, hierarchy_id, parent_id) REFERENCES ledger.hierarchy_node (tenant_id, hierarchy_id, id),
  FOREIGN KEY (tenant_id, change_id) REFERENCES ledger.center_change (tenant_id, id),
  FOREIGN KEY (tenant_id, removed_by_change_id) REFERENCES ledger.center_change (tenant_id, id),
  CONSTRAINT hierarchy_node_root_kept CHECK (parent_id IS NOT NULL OR removed_by_change_id IS NULL)
);
CREATE UNIQUE INDEX hierarchy_node_code_once ON ledger.hierarchy_node (tenant_id, hierarchy_id, code) WHERE removed_by_change_id IS NULL;
CREATE UNIQUE INDEX hierarchy_one_root ON ledger.hierarchy_node (tenant_id, hierarchy_id) WHERE parent_id IS NULL;
CREATE INDEX hierarchy_node_of_hierarchy ON ledger.hierarchy_node (tenant_id, hierarchy_id) WHERE removed_by_change_id IS NULL;
CREATE INDEX hierarchy_node_children ON ledger.hierarchy_node (tenant_id, parent_id) WHERE removed_by_change_id IS NULL;
CREATE TRIGGER hierarchy_node_touch BEFORE UPDATE ON ledger.hierarchy_node FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.hierarchy_node'::regclass);

-- A center's place in a hierarchy. Moving it changes node_id; removing it
-- from an alternative hierarchy sets removed_by_change_id, once.
CREATE TABLE ledger.hierarchy_member (
  tenant_id             uuid NOT NULL,
  id                    uuid NOT NULL,
  hierarchy_id          uuid NOT NULL,
  kind                  text NOT NULL,
  node_id               uuid NOT NULL,
  cost_center_id        uuid,
  profit_center_id      uuid,
  change_id             uuid,               -- the change that placed it; NULL when placed by migration 000128
  removed_by_change_id  uuid,
  created_at            timestamptz NOT NULL DEFAULT now(),
  created_by            uuid NOT NULL,
  updated_at            timestamptz NOT NULL DEFAULT now(),
  updated_by            uuid NOT NULL,
  version               integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT hierarchy_member_kind FOREIGN KEY (tenant_id, hierarchy_id, kind) REFERENCES ledger.hierarchy (tenant_id, id, kind),
  CONSTRAINT hierarchy_member_node FOREIGN KEY (tenant_id, hierarchy_id, node_id) REFERENCES ledger.hierarchy_node (tenant_id, hierarchy_id, id),
  FOREIGN KEY (tenant_id, cost_center_id) REFERENCES ledger.cost_center (tenant_id, id),
  FOREIGN KEY (tenant_id, profit_center_id) REFERENCES ledger.profit_center (tenant_id, id),
  FOREIGN KEY (tenant_id, change_id) REFERENCES ledger.center_change (tenant_id, id),
  FOREIGN KEY (tenant_id, removed_by_change_id) REFERENCES ledger.center_change (tenant_id, id),
  CONSTRAINT hierarchy_member_center CHECK (
    (kind = 'cost_center' AND cost_center_id IS NOT NULL AND profit_center_id IS NULL)
    OR (kind = 'profit_center' AND profit_center_id IS NOT NULL AND cost_center_id IS NULL))
);
CREATE UNIQUE INDEX hierarchy_member_once ON ledger.hierarchy_member (tenant_id, hierarchy_id, coalesce(cost_center_id, profit_center_id))
  WHERE removed_by_change_id IS NULL;
CREATE INDEX hierarchy_member_of_node ON ledger.hierarchy_member (tenant_id, node_id) WHERE removed_by_change_id IS NULL;
CREATE INDEX hierarchy_member_of_center ON ledger.hierarchy_member (tenant_id, coalesce(cost_center_id, profit_center_id)) WHERE removed_by_change_id IS NULL;
CREATE TRIGGER hierarchy_member_touch BEFORE UPDATE ON ledger.hierarchy_member FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('ledger.hierarchy_member'::regclass);

-- The tree rules, written with the hierarchy locked so that two changes
-- to one tree are checked one after the other: a node's parent is a node
-- of the tree that holds no centers; no cycle; at most 12 levels; a root
-- stays the root; a removed node has no children or centers and stays
-- removed.
CREATE FUNCTION ledger.check_hierarchy_node() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  parent_level integer;
  height integer;
BEGIN
  PERFORM FROM ledger.hierarchy WHERE tenant_id = NEW.tenant_id AND id = NEW.hierarchy_id FOR UPDATE;
  IF TG_OP = 'UPDATE' THEN
    IF OLD.removed_by_change_id IS NOT NULL THEN
      RAISE EXCEPTION 'hierarchy_node: a removed node is fixed' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.hierarchy_id IS DISTINCT FROM OLD.hierarchy_id OR (NEW.parent_id IS NULL) <> (OLD.parent_id IS NULL) THEN
      RAISE EXCEPTION 'hierarchy_node: a node stays in its hierarchy, and a root stays the root' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.removed_by_change_id IS NOT NULL AND (
         EXISTS (SELECT FROM ledger.hierarchy_node c WHERE c.tenant_id = NEW.tenant_id AND c.parent_id = NEW.id AND c.removed_by_change_id IS NULL)
         OR EXISTS (SELECT FROM ledger.hierarchy_member m WHERE m.tenant_id = NEW.tenant_id AND m.node_id = NEW.id AND m.removed_by_change_id IS NULL)) THEN
      RAISE EXCEPTION 'hierarchy_node: a node with child nodes or centers cannot be removed' USING ERRCODE = 'check_violation';
    END IF;
  END IF;
  IF NEW.parent_id IS NULL OR NEW.removed_by_change_id IS NOT NULL
     OR (TG_OP = 'UPDATE' AND NEW.parent_id = OLD.parent_id) THEN
    RETURN NEW;
  END IF;
  IF NOT EXISTS (SELECT FROM ledger.hierarchy_node p WHERE p.tenant_id = NEW.tenant_id AND p.id = NEW.parent_id AND p.removed_by_change_id IS NULL) THEN
    RAISE EXCEPTION 'hierarchy_node: the parent node is removed' USING ERRCODE = 'check_violation';
  END IF;
  IF EXISTS (SELECT FROM ledger.hierarchy_member m WHERE m.tenant_id = NEW.tenant_id AND m.node_id = NEW.parent_id AND m.removed_by_change_id IS NULL) THEN
    RAISE EXCEPTION 'hierarchy_node: centers sit at leaves; the parent node holds centers' USING ERRCODE = 'check_violation';
  END IF;
  -- The parent's ancestors, the parent included; reaching the node itself
  -- is a cycle.
  WITH RECURSIVE up (id, parent_id, level) AS (
    SELECT n.id, n.parent_id, 1 FROM ledger.hierarchy_node n WHERE n.tenant_id = NEW.tenant_id AND n.id = NEW.parent_id
    UNION
    SELECT n.id, n.parent_id, up.level + 1 FROM up JOIN ledger.hierarchy_node n ON n.tenant_id = NEW.tenant_id AND n.id = up.parent_id
    WHERE up.id <> NEW.id
  )
  SELECT CASE WHEN bool_or(id = NEW.id) THEN NULL ELSE max(level) END INTO parent_level FROM up;
  IF parent_level IS NULL THEN
    RAISE EXCEPTION 'hierarchy_node: the node would be its own ancestor' USING ERRCODE = 'check_violation';
  END IF;
  WITH RECURSIVE down (id, level) AS (
    SELECT NEW.id, 1
    UNION
    SELECT n.id, down.level + 1 FROM down JOIN ledger.hierarchy_node n
      ON n.tenant_id = NEW.tenant_id AND n.parent_id = down.id AND n.removed_by_change_id IS NULL
  )
  SELECT max(level) INTO height FROM down;
  IF parent_level + height > 12 THEN
    RAISE EXCEPTION 'hierarchy_node: a hierarchy has at most 12 levels' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_hierarchy_node() OWNER TO erp_owner;
CREATE TRIGGER hierarchy_node_tree BEFORE INSERT OR UPDATE ON ledger.hierarchy_node FOR EACH ROW EXECUTE FUNCTION ledger.check_hierarchy_node();

-- A center sits at a leaf that is not removed; what a place names is
-- fixed but its node, and a removed place stays removed.
CREATE FUNCTION ledger.check_hierarchy_member() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM FROM ledger.hierarchy WHERE tenant_id = NEW.tenant_id AND id = NEW.hierarchy_id FOR UPDATE;
  IF TG_OP = 'UPDATE' AND (OLD.removed_by_change_id IS NOT NULL
     OR (NEW.hierarchy_id, NEW.kind, NEW.cost_center_id, NEW.profit_center_id) IS DISTINCT FROM (OLD.hierarchy_id, OLD.kind, OLD.cost_center_id, OLD.profit_center_id)) THEN
    RAISE EXCEPTION 'hierarchy_member: a place changes only its node, until it is removed' USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.removed_by_change_id IS NULL AND (
       EXISTS (SELECT FROM ledger.hierarchy_node c WHERE c.tenant_id = NEW.tenant_id AND c.parent_id = NEW.node_id AND c.removed_by_change_id IS NULL)
       OR NOT EXISTS (SELECT FROM ledger.hierarchy_node n WHERE n.tenant_id = NEW.tenant_id AND n.id = NEW.node_id AND n.removed_by_change_id IS NULL)) THEN
    RAISE EXCEPTION 'hierarchy_member: a center sits at a leaf node that is not removed' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION ledger.check_hierarchy_member() OWNER TO erp_owner;
CREATE TRIGGER hierarchy_member_leaf BEFORE INSERT OR UPDATE ON ledger.hierarchy_member FOR EACH ROW EXECUTE FUNCTION ledger.check_hierarchy_member();

-- Every active center is in the standard hierarchy of its kind, checked
-- at commit so that a center and its place are written in one
-- transaction: when a center becomes active, and when a place in a
-- standard hierarchy moves away or is removed.
CREATE FUNCTION ledger.check_center_placed() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  v_tenant uuid;
  v_center uuid;
  v_kind text;
BEGIN
  IF TG_TABLE_NAME = 'hierarchy_member' THEN
    v_tenant := OLD.tenant_id;
    v_center := coalesce(OLD.cost_center_id, OLD.profit_center_id);
    v_kind := OLD.kind;
  ELSE
    v_tenant := NEW.tenant_id;
    v_center := NEW.id;
    v_kind := TG_TABLE_NAME;
  END IF;
  IF EXISTS (SELECT FROM ledger.cost_center c WHERE v_kind = 'cost_center' AND c.tenant_id = v_tenant AND c.id = v_center AND c.status = 'active'
             UNION ALL
             SELECT FROM ledger.profit_center c WHERE v_kind = 'profit_center' AND c.tenant_id = v_tenant AND c.id = v_center AND c.status = 'active')
     AND NOT EXISTS (SELECT FROM ledger.hierarchy_member m JOIN ledger.hierarchy h ON h.tenant_id = m.tenant_id AND h.id = m.hierarchy_id
                     WHERE m.tenant_id = v_tenant AND coalesce(m.cost_center_id, m.profit_center_id) = v_center AND m.kind = v_kind
                       AND m.removed_by_change_id IS NULL AND h.is_standard) THEN
    RAISE EXCEPTION '%: an active center sits in the standard hierarchy of its kind', TG_TABLE_NAME USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;
ALTER FUNCTION ledger.check_center_placed() OWNER TO erp_owner;

-- center_change: a hierarchy, and the kinds that change one. A node change
-- names no center; placing a center names the center and the hierarchy.
ALTER TABLE ledger.center_change ADD COLUMN hierarchy_id uuid,
  ADD CONSTRAINT center_change_hierarchy_fk FOREIGN KEY (tenant_id, hierarchy_id) REFERENCES ledger.hierarchy (tenant_id, id);
ALTER TABLE ledger.center_change DROP CONSTRAINT center_change_kind_check,
  ADD CONSTRAINT center_change_kind_check CHECK (kind IN ('create', 'redate', 'lock', 'move', 'assign',
    'node_create', 'node_rename', 'node_move', 'node_remove', 'place'));
ALTER TABLE ledger.center_change DROP CONSTRAINT center_change_one_center,
  ADD CONSTRAINT center_change_one_center CHECK (num_nonnulls(cost_center_id, profit_center_id) = CASE WHEN kind LIKE 'node\_%' THEN 0 ELSE 1 END),
  ADD CONSTRAINT center_change_hierarchy CHECK ((hierarchy_id IS NOT NULL) = (kind LIKE 'node\_%' OR kind = 'place'));
CREATE INDEX center_change_by_hierarchy ON ledger.center_change (tenant_id, hierarchy_id) WHERE hierarchy_id IS NOT NULL;

CREATE OR REPLACE FUNCTION ledger.check_center_change_decision() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF (NEW.cost_center_id, NEW.profit_center_id, NEW.hierarchy_id, NEW.kind, NEW.payload, NEW.reason, NEW.requested_by)
     IS DISTINCT FROM (OLD.cost_center_id, OLD.profit_center_id, OLD.hierarchy_id, OLD.kind, OLD.payload, OLD.reason, OLD.requested_by) THEN
    RAISE EXCEPTION 'center_change: what a change asks for is fixed' USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.status IS DISTINCT FROM OLD.status AND OLD.status <> 'pending' THEN
    RAISE EXCEPTION 'center_change: a change cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.workflow_request_id IS DISTINCT FROM OLD.workflow_request_id AND OLD.workflow_request_id IS NOT NULL THEN
    RAISE EXCEPTION 'center_change: the approval request of a change is fixed' USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;

-- Centers that exist before T56b: each tenant with centers of a kind gets
-- a standard hierarchy STANDARD with one leaf UNASSIGNED holding them, for
-- a controller to sort (ADR-0050 as built in T56b). Only active centers
-- are placed: a create still pending names no node, so its approval is
-- refused and it is requested again (T56a was not released).
WITH kinds (tenant_id, kind, actor) AS (
  SELECT tenant_id, 'cost_center', (array_agg(created_by ORDER BY created_at, id))[1] FROM ledger.cost_center WHERE status = 'active' GROUP BY tenant_id
  UNION ALL
  SELECT tenant_id, 'profit_center', (array_agg(created_by ORDER BY created_at, id))[1] FROM ledger.profit_center WHERE status = 'active' GROUP BY tenant_id
), hierarchies AS (
  INSERT INTO ledger.hierarchy (tenant_id, id, kind, code, name, is_standard, created_by, updated_by)
  SELECT tenant_id, gen_random_uuid(), kind, 'STANDARD', 'Standard hierarchy', true, actor, actor FROM kinds
  RETURNING tenant_id, id, kind, created_by
), roots AS (
  INSERT INTO ledger.hierarchy_node (tenant_id, id, hierarchy_id, code, name, created_by, updated_by)
  SELECT tenant_id, gen_random_uuid(), id, 'STANDARD', 'Standard hierarchy', created_by, created_by FROM hierarchies
  RETURNING tenant_id, id, hierarchy_id
)
INSERT INTO ledger.hierarchy_node (tenant_id, id, hierarchy_id, parent_id, code, name, created_by, updated_by)
SELECT r.tenant_id, gen_random_uuid(), r.hierarchy_id, r.id, 'UNASSIGNED', 'Unassigned', h.created_by, h.created_by
FROM roots r JOIN hierarchies h ON h.tenant_id = r.tenant_id AND h.id = r.hierarchy_id;

INSERT INTO ledger.hierarchy_member (tenant_id, id, hierarchy_id, kind, node_id, cost_center_id, profit_center_id, created_by, updated_by)
SELECT n.tenant_id, gen_random_uuid(), h.id, h.kind, n.id, CASE WHEN h.kind = 'cost_center' THEN c.id END,
       CASE WHEN h.kind = 'profit_center' THEN c.id END, c.created_by, c.created_by
FROM ledger.hierarchy h
JOIN ledger.hierarchy_node n ON n.tenant_id = h.tenant_id AND n.hierarchy_id = h.id AND n.code = 'UNASSIGNED'
JOIN (SELECT tenant_id, id, created_by, 'cost_center' AS kind FROM ledger.cost_center WHERE status = 'active'
      UNION ALL
      SELECT tenant_id, id, created_by, 'profit_center' FROM ledger.profit_center WHERE status = 'active') c
  ON c.tenant_id = h.tenant_id AND c.kind = h.kind
WHERE h.is_standard;

CREATE CONSTRAINT TRIGGER cost_center_placed AFTER INSERT OR UPDATE OF status ON ledger.cost_center
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger.check_center_placed();
CREATE CONSTRAINT TRIGGER profit_center_placed AFTER INSERT OR UPDATE OF status ON ledger.profit_center
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger.check_center_placed();
CREATE CONSTRAINT TRIGGER hierarchy_member_placed AFTER UPDATE OR DELETE ON ledger.hierarchy_member
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger.check_center_placed();

UPDATE ref.permission SET description = 'Approve center changes: creation, validity, locks, profit center, segment, companies and hierarchies'
WHERE code = 'ledger.cost_center.approve';
