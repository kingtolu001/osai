package ledgergrpc

// Sandbox internal claim transfers have zero net customer cash flow and are
// excluded. FX has one independently recorded cash-flow row per currency;
// amounts are never summed across currencies or against another owner.
const customerTransactionRead = `WITH customer_journals AS (
 SELECT e.journal_id,j.trade_id,e.currency,j.created_at,j.kind,
 SUM(CASE
 WHEN $2 AND j.kind LIKE 'sandbox.%' AND e.owner=$1 AND e.account_code IN ('sbx:'||e.currency||':available','sbx:'||e.currency||':held','sbx:'||e.currency||':restricted') THEN e.credit-e.debit
 WHEN j.kind='prefund' AND e.account_code='2000' THEN e.credit-e.debit
 WHEN j.kind='settlement.failed' AND e.account_code='2000' THEN e.credit-e.debit
 WHEN j.kind='settlement.confirmed' AND e.account_code='2100' THEN -e.debit
 WHEN j.kind='legacy' AND e.account_code='2200' THEN -e.debit ELSE 0 END) AS amount_minor
 FROM ledger_entries e JOIN ledger_journals j ON j.journal_id=e.journal_id
 WHERE j.institution_id=$1 AND j.kind<>'hold' AND (j.kind NOT LIKE 'sandbox.%' OR $2)
 GROUP BY e.journal_id,j.trade_id,e.currency,j.created_at,j.kind
), visible AS (SELECT * FROM customer_journals WHERE kind NOT LIKE 'sandbox.%' OR amount_minor<>0)`
