-- Direct uploads (T16): documents above the request limit (04 §4.11) go
-- straight to object storage through a URL the API hands out, and are then
-- named by the operation that consumes them (:capture). The row records what
-- was promised (type, size limit, where the bytes go, until when) and what
-- became of it.
CREATE TABLE payables.upload (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL,
  purpose       text NOT NULL CHECK (purpose IN ('supplier_invoice')),
  content_type  text NOT NULL,
  max_bytes     bigint NOT NULL CHECK (max_bytes > 0),
  object_ref    text NOT NULL,
  status        text NOT NULL CHECK (status IN ('open', 'received', 'used')),
  size          bigint,
  sha256        bytea,
  used_by       uuid,                -- the supplier invoice captured from it
  expires_at    timestamptz NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  created_by    uuid NOT NULL,
  updated_at    timestamptz NOT NULL DEFAULT now(),
  version       integer NOT NULL DEFAULT 1,
  PRIMARY KEY (tenant_id, id),
  CHECK ((status = 'used') = (used_by IS NOT NULL))
);
CREATE TRIGGER upload_touch BEFORE UPDATE ON payables.upload FOR EACH ROW EXECUTE FUNCTION platform.touch();
SELECT platform.secure_table('payables.upload'::regclass);
