-- Personal data blocked (T100b; ADR-0079 §11; 03-data-model §3.6): the
-- privacy officer blocks a business partner's personal data once nothing
-- is open for it. The partner is blocked with it, and its reads answer its
-- name, addresses, tax identifiers and bank accounts masked to a caller
-- without partner.privacy.read_blocked. Expand only (06 §6.4): a nullable
-- column, a check every existing row meets and two permissions, which the
-- previous version neither reads nor writes.
ALTER TABLE partner.business_partner
  ADD COLUMN personal_data_blocked_at timestamptz,
  -- A partner whose personal data is blocked is blocked itself: no
  -- document is entered or posted for it.
  ADD CONSTRAINT business_partner_personal_data_blocked_check CHECK (personal_data_blocked_at IS NULL OR status = 'blocked');

INSERT INTO ref.permission (code, description, is_approval) VALUES
  ('partner.privacy.block', 'Block and unblock a business partner''s personal data', false),
  ('partner.privacy.read_blocked', 'Read the personal data of a business partner whose personal data is blocked', false);
