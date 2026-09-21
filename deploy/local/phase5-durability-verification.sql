-- Verification only. No business data or schema mutations.
BEGIN TRANSACTION READ ONLY;
SELECT current_database(), current_timestamp;
SELECT table_schema, table_name
FROM information_schema.tables
WHERE table_schema NOT IN ('pg_catalog', 'information_schema')
ORDER BY 1, 2;
SELECT table_name, column_name, data_type
FROM information_schema.columns
WHERE table_schema = 'public'
  AND table_name IN ('customer_api_idempotency', 'settlement_instructions')
ORDER BY table_name, ordinal_position;
-- Deliberately omit credential material, payloads, and response bodies.
SELECT institution_id, endpoint, count(*) AS records
FROM customer_api_idempotency
WHERE institution_id = 'inst_sandbox_local'
GROUP BY institution_id, endpoint;
SELECT count(*) AS settlement_rows FROM settlement_instructions;
SELECT count(*) AS supplied_trace_quote_records
FROM customer_api_idempotency
WHERE institution_id = 'inst_sandbox_local'
  AND response_body->>'quote_id' = 'quo_068a0f23d3f3';
SELECT count(*) AS supplied_trace_trade_records
FROM customer_api_idempotency
WHERE institution_id = 'inst_sandbox_local'
  AND response_body->>'trade_id' = 'trd_c0ce2556-b68a-4f2e-a418-788244cbbd6c';
ROLLBACK;
