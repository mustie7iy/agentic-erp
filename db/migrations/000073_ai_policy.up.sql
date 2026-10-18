-- A tenant's AI policy (T42d, ADR-0036, 03-data-model §3.14): whether its
-- model calls and agents run, where its model aliases route, which models
-- it allows, its monthly cap and whether documents may go to models. One
-- row per tenant, created on its first change; a tenant without one has
-- the defaults: AI on, the platform's routes, any priced model, no cap,
-- documents allowed.
CREATE TABLE agents.ai_policy (
  tenant_id            uuid PRIMARY KEY,
  -- The kill switch has two hands: the tenant's administrators (enabled)
  -- and the platform's operators (suspended, on the control plane). AI
  -- runs only while enabled and not suspended: the operator's wins.
  enabled              boolean NOT NULL DEFAULT true,
  suspended            boolean NOT NULL DEFAULT false,
  suspended_reason     text CHECK (length(suspended_reason) BETWEEN 1 AND 500),
  -- The tenant's monthly cap on model spending, in minor units of the
  -- prices' currency; NULL: none.
  monthly_cap          bigint CONSTRAINT ai_policy_monthly_cap_check CHECK (monthly_cap >= 0),
  cap_currency         char(3) REFERENCES ref.currency CONSTRAINT ai_policy_cap_currency_check CHECK (cap_currency = 'USD'),
  -- Model alias → model, over the platform's routes.
  routes               jsonb NOT NULL DEFAULT '{}' CONSTRAINT ai_policy_routes_check CHECK (jsonb_typeof(routes) = 'object'),
  -- The models the tenant's aliases may route to; empty: any priced model.
  allowed_models       text[] NOT NULL DEFAULT '{}',
  documents_to_models  boolean NOT NULL DEFAULT true,
  updated_at           timestamptz NOT NULL DEFAULT now(),
  updated_by           uuid,            -- NULL: the defaults, never changed
  version              integer NOT NULL DEFAULT 1,
  CONSTRAINT ai_policy_suspension_reason CHECK (suspended = (suspended_reason IS NOT NULL)),
  CONSTRAINT ai_policy_cap_currency CHECK ((monthly_cap IS NULL) = (cap_currency IS NULL))
);
CREATE TRIGGER ai_policy_touch BEFORE UPDATE ON agents.ai_policy FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('agents.ai_policy'::regclass);

-- Reading and changing the policy are permissions of their own; agents may
-- hold neither (x-agent: false).
INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('agents.ai_policy.read', 'Read the tenant''s AI policy', false),
  ('agents.ai_policy.manage', 'Change the tenant''s AI policy and switch its AI on or off', false);
