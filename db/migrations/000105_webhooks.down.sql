DELETE FROM ref.permission WHERE code IN ('integration.webhook.read', 'integration.webhook.manage');
DROP TABLE integration.webhook_delivery;
DROP TABLE integration.webhook_subscription;
