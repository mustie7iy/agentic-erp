DROP TABLE IF EXISTS config.inbox, config.outbox, config.custom_field_schema, config.number_range, config.evidence,
  config.change_item, config.change_request, config.active_version, config.config_version CASCADE;
DROP FUNCTION IF EXISTS config.next_document_number(uuid, uuid, text, smallint);
