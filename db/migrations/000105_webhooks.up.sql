-- Webhook subscriptions and their deliveries (T50c, ADR-0041; 03-data-model
-- §3.13). A subscription sends a tenant's events of the listed types to one
-- HTTPS endpoint, signed with its own whsec_ secret sealed by fieldcrypt
-- (ADR-0018). At most 20 live subscriptions per tenant is the application's
-- rule (a count under the tenant's lock), not a constraint.
-- The URL has a host without credentials and no fragment; a query may carry
-- the endpoint's token.
CREATE TABLE integration.webhook_subscription (
  tenant_id               uuid NOT NULL,
  id                      uuid NOT NULL,
  url                     text NOT NULL CHECK (url ~ '^https://[^\s/?#@]+([/?][^\s#]*)?$' AND length(url) <= 2048),
  description             text NOT NULL DEFAULT '' CHECK (length(description) <= 200),
  -- Full CloudEvents types or `<context>.*`; that each is offered for
  -- webhooks (x-webhook in api/asyncapi.yaml) is checked by the application.
  -- A NULL element becomes '' and fails the pattern.
  event_types             text[] NOT NULL CHECK (cardinality(event_types) BETWEEN 1 AND 100
                            AND array_to_string(event_types, ',', '') ~ '^([a-z_]+\.\*|com\.example\.erp\.[a-z_]+\.[a-z_]+\.v[0-9]+)(,([a-z_]+\.\*|com\.example\.erp\.[a-z_]+\.[a-z_]+\.v[0-9]+))*$'),
  -- Nothing is deleted (03 §3.1): DELETE ends a subscription as `deleted`.
  status                  text NOT NULL CHECK (status IN ('active', 'paused', 'disabled', 'deleted')),
  -- Why the dispatcher disabled it: failing for 5 days, or a 410 Gone.
  disabled_reason         text CHECK (disabled_reason IN ('failing', 'gone')),
  failing_since           timestamptz,   -- first failed delivery since the last success
  secret_cipher           bytea NOT NULL,
  -- During a rotation the old secret also signs until it expires (24 h).
  previous_secret_cipher  bytea,
  previous_expires_at     timestamptz,
  version                 integer NOT NULL DEFAULT 1,
  created_at              timestamptz NOT NULL DEFAULT now(),
  created_by              uuid NOT NULL,
  updated_at              timestamptz NOT NULL DEFAULT now(),
  updated_by              uuid NOT NULL,
  PRIMARY KEY (tenant_id, id),
  CONSTRAINT webhook_subscription_disabled_reason CHECK ((status = 'disabled') = (disabled_reason IS NOT NULL)),
  CONSTRAINT webhook_subscription_previous_secret CHECK ((previous_secret_cipher IS NULL) = (previous_expires_at IS NULL))
);
CREATE TRIGGER webhook_subscription_touch BEFORE UPDATE ON integration.webhook_subscription FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('integration.webhook_subscription'::regclass);

-- One delivery per event and subscription: fan-out is idempotent on the
-- unique key. Kept 30 days; the dispatcher's retention removes older ones.
CREATE TABLE integration.webhook_delivery (
  tenant_id        uuid NOT NULL,
  id               uuid NOT NULL,
  subscription_id  uuid NOT NULL,
  event_id         uuid NOT NULL,   -- the CloudEvents id (outbox row id)
  type             text NOT NULL CHECK (type ~ '^com\.example\.erp\.[a-z_]+\.[a-z_]+\.v[0-9]+$'),
  subject          uuid NOT NULL,   -- deliveries are ordered per subject
  body             bytea NOT NULL,  -- the structured-mode CloudEvent, sent byte for byte on every attempt
  status           text NOT NULL CHECK (status IN ('pending', 'delivered', 'dead')),
  attempts         integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  next_attempt_at  timestamptz,
  last_status      integer CHECK (last_status BETWEEN 100 AND 599),  -- the endpoint's last HTTP status
  last_error       text CHECK (length(last_error) <= 1000),          -- why the last attempt failed without one
  delivered_at     timestamptz,
  version          integer NOT NULL DEFAULT 1,
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, id),
  UNIQUE (tenant_id, subscription_id, event_id),
  FOREIGN KEY (tenant_id, subscription_id) REFERENCES integration.webhook_subscription (tenant_id, id),
  CONSTRAINT webhook_delivery_next_attempt CHECK ((status = 'pending') = (next_attempt_at IS NOT NULL)),
  CONSTRAINT webhook_delivery_delivered CHECK ((status = 'delivered') = (delivered_at IS NOT NULL)),
  -- Only a pending delivery may not have been attempted yet.
  CONSTRAINT webhook_delivery_attempted CHECK (status = 'pending' OR attempts >= 1)
);
-- The dispatcher picks the pending deliveries that are due.
CREATE INDEX webhook_delivery_due ON integration.webhook_delivery (next_attempt_at) WHERE status = 'pending';
CREATE TRIGGER webhook_delivery_touch BEFORE UPDATE ON integration.webhook_delivery FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('integration.webhook_delivery'::regclass);

INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('integration.webhook.read', 'View webhook subscriptions and deliveries', false),
  ('integration.webhook.manage', 'Create, change, rotate and delete webhook subscriptions', false);
