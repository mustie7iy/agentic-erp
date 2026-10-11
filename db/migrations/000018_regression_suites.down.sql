DROP TABLE config.regression_case;
DROP TABLE config.regression_suite;
DELETE FROM ref.permission WHERE code = 'config.regression.manage';
