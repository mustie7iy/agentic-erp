-- A house bank's channel (T44b; ADR-0038; 03-data-model §3.10): the
-- integration connector its bank files are transmitted through, or null for
-- the server's directory (DirConnector). The connector is the integration
-- context's, so no foreign key holds the reference (03-data-model §3.1);
-- connectors are never deleted, and transmission refuses one it does not
-- find. The column is nullable without a default (06 §6.4): the previous
-- version neither reads nor writes it, so a configuration naming a
-- connector waits until the upgrade completes, as configuration changes do.
ALTER TABLE payments.house_bank ADD COLUMN connector_id uuid;
