-- The model gateway's ledger (T42b, ADR-0036, 03-data-model §3.14): what
-- each model costs, and one row per call the gateway made. A row records
-- who called, for which session, the alias asked for and the model that
-- answered, the tokens and the cost, and never content. Costs are
-- money.Micro: millionths of the currency's minor unit (ADR-0005 as
-- amended).
CREATE TABLE ref.model_price (
  model           text NOT NULL CHECK (model ~ '^[a-z0-9][a-z0-9.-]{0,99}$'),
  effective_from  date NOT NULL,
  currency        char(3) NOT NULL REFERENCES ref.currency,
  -- Per million tokens, in millionths of the minor unit.
  input           bigint NOT NULL CONSTRAINT model_price_input_check CHECK (input >= 0),
  output          bigint NOT NULL CHECK (output >= 0),
  cache_write     bigint NOT NULL CHECK (cache_write >= 0),
  cache_read      bigint NOT NULL CHECK (cache_read >= 0),
  PRIMARY KEY (model, effective_from)
);
ALTER TABLE ref.model_price OWNER TO erp_owner;
GRANT SELECT ON ref.model_price TO erp_app, erp_readonly;

-- Anthropic's first-party prices of 2026-10-06, USD per million tokens:
-- input, output, 5-minute cache write (1.25 × input), cache read.
INSERT INTO ref.model_price (model, effective_from, currency, input, output, cache_write, cache_read) VALUES
  ('claude-opus-5-5',   '2026-10-01', 'USD', 400000000, 2000000000, 500000000, 20000000),
  ('claude-sonnet-5-5', '2026-10-01', 'USD', 200000000, 1000000000, 250000000, 20000000),
  ('claude-haiku-5-5',  '2026-10-01', 'USD',  10000000,   50000000,  12500000,  1000000);

CREATE TABLE agents.model_call (
  tenant_id           uuid NOT NULL,
  id                  uuid NOT NULL,
  principal_id        uuid NOT NULL,          -- who called: an agent, or the person whose request a server-side call served
  session_id          uuid,                   -- the agent's session, when it named one
  alias               text NOT NULL CHECK (alias ~ '^[a-z0-9][a-z0-9.-]{0,99}$'),
  model               text NOT NULL,          -- the model that answered
  status              text NOT NULL CONSTRAINT model_call_status_check CHECK (status IN ('succeeded', 'refused', 'failed')),
  input_tokens        bigint NOT NULL CONSTRAINT model_call_input_tokens_check CHECK (input_tokens >= 0),
  output_tokens       bigint NOT NULL CHECK (output_tokens >= 0),
  cache_write_tokens  bigint NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),
  cache_read_tokens   bigint NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0),
  cost_micro          bigint NOT NULL CHECK (cost_micro >= 0),
  currency            char(3) NOT NULL REFERENCES ref.currency,
  duration_ms         integer NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
  called_at           timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  FOREIGN KEY (tenant_id, session_id) REFERENCES agents.session (tenant_id, id)
);
CREATE INDEX model_call_by_principal ON agents.model_call (tenant_id, principal_id, called_at);
CREATE INDEX model_call_by_session ON agents.model_call (tenant_id, session_id) WHERE session_id IS NOT NULL;
CREATE TRIGGER model_call_immutable BEFORE UPDATE OR DELETE ON agents.model_call FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
SELECT platform.secure_table('agents.model_call'::regclass);

-- A session's cost is the sum of its calls', exact; cost keeps it in minor
-- units for the API.
ALTER TABLE agents.session ADD COLUMN cost_micro bigint NOT NULL DEFAULT 0 CONSTRAINT session_cost_micro_check CHECK (cost_micro >= 0);
