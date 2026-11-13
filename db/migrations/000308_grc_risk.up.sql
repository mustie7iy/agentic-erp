-- The risk register (T98c; ADR-0078 §10-§12; 03-data-model §3.31): the
-- tenant's risks, each with its owner, its latest likelihood and impact
-- and the controls of the library that mitigate it; the history of its
-- ratings, insert-only. owner_id, rated_by and the other principals are
-- the identity context's: no foreign key leaves the schema (02 §2.1).
-- Expand only: new tables and two permissions, which the previous version
-- neither reads nor writes (06 §6.4).

-- A risk (ADR-0078 §10): a title, a category in the tenant's own words,
-- the person who owns it, and the likelihood and impact of its latest
-- rating, from 1 to 5 each, whose product is its score. A risk is open
-- or closed; it is closed once, with a reason, and never deleted.
CREATE TABLE grc.risk (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,
  title         text NOT NULL CONSTRAINT risk_title_check CHECK (length(title) BETWEEN 1 AND 200 AND title = btrim(title)),
  category      text NOT NULL CONSTRAINT risk_category_check CHECK (length(category) BETWEEN 1 AND 100 AND category = btrim(category)),
  owner_id      uuid NOT NULL,       -- identity context: a person
  likelihood    smallint NOT NULL CONSTRAINT risk_likelihood_check CHECK (likelihood BETWEEN 1 AND 5),
  impact        smallint NOT NULL CONSTRAINT risk_impact_check CHECK (impact BETWEEN 1 AND 5),
  score         smallint GENERATED ALWAYS AS (likelihood * impact) STORED,
  status        text NOT NULL DEFAULT 'open' CONSTRAINT risk_status_check CHECK (status IN ('open', 'closed')),
  close_reason  text CONSTRAINT risk_close_reason_check CHECK (length(btrim(close_reason)) > 0 AND length(close_reason) <= 1000),
  closed_at     timestamptz,
  closed_by     uuid,                -- identity context: the person who closed it
  version       integer NOT NULL DEFAULT 1 CONSTRAINT risk_version_check CHECK (version >= 1),
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  updated_by    uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  -- A closed risk says why, when and by whom; an open one none of these.
  CONSTRAINT risk_closed_check CHECK ((status = 'closed') = (close_reason IS NOT NULL)
    AND (close_reason IS NULL) = (closed_at IS NULL) AND (close_reason IS NULL) = (closed_by IS NULL))
);
CREATE TRIGGER risk_touch BEFORE UPDATE ON grc.risk FOR EACH ROW EXECUTE FUNCTION platform.touch();
-- The register: the tenant's risks by score, the highest first, then by
-- title, in the order its pages are read.
CREATE INDEX risk_by_score ON grc.risk (tenant_id, score DESC, title, id);
SELECT platform.secure_table('grc.risk'::regclass);

-- A closed risk is fixed: closing is its last change.
CREATE FUNCTION grc.check_risk_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'closed' THEN
    RAISE EXCEPTION 'risk %: a closed risk is not changed (risk_closed_fixed)', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'risk_closed_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION grc.check_risk_change() OWNER TO erp_owner;
CREATE TRIGGER risk_closed_fixed BEFORE UPDATE ON grc.risk
  FOR EACH ROW EXECUTE FUNCTION grc.check_risk_change();
-- A change sets what a risk says, its latest rating and its closing; no
-- risk is deleted.
REVOKE UPDATE ON grc.risk FROM erp_app;
GRANT UPDATE (title, category, owner_id, likelihood, impact, status, close_reason, closed_at, closed_by, version, updated_by) ON grc.risk TO erp_app;

-- A rating of a risk (ADR-0078 §11): its likelihood and impact from 1 to
-- 5, who rated and when. Insert-only: the history of a risk's ratings.
-- The latest, by id (a UUIDv7), is the risk's, which the index reads
-- first.
CREATE TABLE grc.risk_rating (
  tenant_id   uuid NOT NULL,
  id          uuid NOT NULL,
  risk_id     uuid NOT NULL,
  likelihood  smallint NOT NULL CONSTRAINT risk_rating_likelihood_check CHECK (likelihood BETWEEN 1 AND 5),
  impact      smallint NOT NULL CONSTRAINT risk_rating_impact_check CHECK (impact BETWEEN 1 AND 5),
  rated_by    uuid NOT NULL,       -- identity context: the person who rated
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT risk_rating_risk_fkey FOREIGN KEY (tenant_id, risk_id) REFERENCES grc.risk (tenant_id, id)
);
CREATE INDEX risk_rating_latest ON grc.risk_rating (tenant_id, risk_id, id DESC);
CREATE TRIGGER risk_rating_immutable BEFORE UPDATE OR DELETE ON grc.risk_rating
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('grc.risk_rating'::regclass);

-- A control of the library that mitigates a risk (ADR-0078 §12). It is
-- active or removed; removed once, when a change of the risk leaves the
-- control out, and never deleted. A risk names a control once at a time.
CREATE TABLE grc.risk_control (
  tenant_id   uuid NOT NULL,
  id          uuid NOT NULL,
  risk_id     uuid NOT NULL,
  control_id  uuid NOT NULL,
  status      text NOT NULL DEFAULT 'active' CONSTRAINT risk_control_status_check CHECK (status IN ('active', 'removed')),
  created_at  timestamptz NOT NULL DEFAULT now(),
  created_by  uuid NOT NULL,
  removed_at  timestamptz,
  removed_by  uuid,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT risk_control_risk_fkey FOREIGN KEY (tenant_id, risk_id) REFERENCES grc.risk (tenant_id, id),
  CONSTRAINT risk_control_control_fkey FOREIGN KEY (tenant_id, control_id) REFERENCES grc.control (tenant_id, id),
  -- A removed control says when and by whom; an active one neither.
  CONSTRAINT risk_control_removed_check CHECK ((status = 'removed') = (removed_at IS NOT NULL) AND (removed_at IS NULL) = (removed_by IS NULL))
);
CREATE UNIQUE INDEX risk_control_once ON grc.risk_control (tenant_id, risk_id, control_id) WHERE status = 'active';
-- The risks a control mitigates (the register's filter by control).
CREATE INDEX risk_control_by_control ON grc.risk_control (tenant_id, control_id) WHERE status = 'active';
SELECT platform.secure_table('grc.risk_control'::regclass);

-- A removed control of a risk is fixed: removing is its last change.
CREATE FUNCTION grc.check_risk_control_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.status = 'removed' THEN
    RAISE EXCEPTION 'risk control %: a removed control of a risk is not changed (risk_control_removed_fixed)', OLD.id
      USING ERRCODE = 'check_violation', CONSTRAINT = 'risk_control_removed_fixed';
  END IF;
  RETURN NEW;
END $$;
ALTER FUNCTION grc.check_risk_control_change() OWNER TO erp_owner;
CREATE TRIGGER risk_control_removed_fixed BEFORE UPDATE ON grc.risk_control
  FOR EACH ROW EXECUTE FUNCTION grc.check_risk_control_change();
-- Removing is the only change: which risk and control stay.
REVOKE UPDATE ON grc.risk_control FROM erp_app;
GRANT UPDATE (status, removed_at, removed_by) ON grc.risk_control TO erp_app;

-- Reading the risk register, and recording, rating, changing and closing
-- risks (ADR-0078 §13).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('grc.risk.read', 'View the risk register and the ratings of its risks', false),
  ('grc.risk.write', 'Record, rate, change and close risks', false);
