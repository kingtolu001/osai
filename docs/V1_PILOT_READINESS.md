# V1 Pilot Readiness

Status: **NOT PILOT-READY** (assessment updated 2026-10-12). Passing unit,
database-backed, and local independent-services tests do not certify
production readiness. The latest independent-services acceptance run passed
for its exercised customer flow, including provider status, Customer restart,
Settlement event publication, and filtered trade/report routes. Finance
approval, executable cross-currency funding/FX, operational controls, and
production reliability gates remain open.

## Gate assessment

| Gate | Verdict | Evidence / remaining work |
| --- | --- | --- |
| Ledger financial invariants | PASS for tested flows | Integer minor units, journal invariants, persisted ledger-derived customer reads, and PostgreSQL tests pass. Broader production integrity monitoring remains unproven. |
| FX pricing, execution, and accounting | FAIL / blocked | Unsupported cross-currency and source/output-mismatched trades fail closed before trade creation or funding. No durable FX execution/posting flow, approved account mappings, inventory cost basis, or fee/gain recognition policy exists. The role-based proposal and review requirements are in [FX_POSITION_AND_LEDGER_POLICY_REVIEW.md](./FX_POSITION_AND_LEDGER_POLICY_REVIEW.md); it is not Finance approval. |
| Customer API contract | PARTIAL | `GET /v1/trades` and `GET /v1/reports/settlements` use owner gRPC APIs for tenant-scoped filtering and pagination; the independent-services test now exercises date, currency, status, customer-reference, and page filters. `GET /v1/providers/status` requires `providers:read` and returns normalized route IDs, availability, and capabilities; the route is exercised independently. The user-defined `total_cost_minor` formula is not exposed: quote/trade/ledger still do not implement source-currency funding plus fee or verified FX execution/accounting. |
| Authentication and authorization | PARTIAL | Customer/KYB persists environment-scoped credentials, entitlements, and webhook configuration; the independent-services run proves a seeded credential remains usable after Customer restart. Public create/list/get/rotate/revoke routes use owner gRPC, but their complete lifecycle is not yet exercised through independent services. Gateway enforces per-institution and per-credential token buckets and route entitlements. Multi-gateway rate-limit aggregation, full service identity/private connectivity, and broader security review remain outstanding. |
| API idempotency | PASS for covered mutations | Durable reservation, payload conflict detection, original response replay, and database-backed tests exist. Webhook replay additionally has owner-side durable idempotency and concurrent duplicate coverage. Failure/concurrency coverage across all public mutations still needs independent-services verification. |
| Quote/trade recovery | PASS for exercised flows | Owner-local persistence, settlement finality outbox and trade inbox are exercised by the current-revision independent-services proof, including restarts, replay and UNKNOWN recovery. Trade-list and settlement-report queries are exercised independently with their supported filters and pagination. |
| Customer webhook security/delivery | PARTIAL | Signed delivery, encrypted configuration, durable retries/dead-letter, and restart recovery are implemented and unit/PostgreSQL tested. The public replay endpoint and durable replay history are implemented and tested for duplicate/concurrent keys, conflict, institution scope and store restart. Quote-expired, trade accepted/funding-required/funded, and Settlement-owned settlement-processing/successfully-settled events have owner-local transactional outboxes and relays; FX-executed, failed/reversed, and customer-relevant reconciliation-break publication remain blocked on authoritative state/evidence and tenant identity. Delivery-history read and operations workflows remain incomplete. |
| SSRF and provider URL controls | PARTIAL | Customer webhook requests reject non-public production targets and redirects. The wider provider URL allowlist and a full security assessment remain unproven. |
| Maker-checker and audit integrity | PARTIAL | Beneficiary maker-checker and append-only action records are covered. Other sensitive actions and hash-chained audit anchoring are not proven. |
| Metrics, alerts, dashboards and runbooks | FAIL | Required metrics/alerts are not fully wired, and required incident runbooks have not been exercised end-to-end. |
| Backup, restore and rebuild | FAIL | No qualifying database restore, balance rebuild, event replay, or reporting reconstruction drill is recorded. |
| Load and failure testing | FAIL | No complete quote burst, callback burst, DB exhaustion, slow dependency, Kafka/Temporal outage or instance-termination matrix has passed. |
| Independent services acceptance | PASS for exercised flow; FX and credential lifecycle scope remain unproven | Current-revision run passed on 2026-10-12 in 279.43s with PostgreSQL and Temporal. It exercised provider status, Customer and Notification restarts, signed webhook retry, Settlement-owned events, trade and settlement filters, callback replay/order, UNKNOWN recovery, funding/hold invariants, and hold release. It does not prove quote funding with fees, cross-currency FX accounting, public credential create/rotate/revoke, or production controls. |

## Recent local test evidence

- `go test -p 1 -ldflags=-w -count=1 ./services/notification-webhook/...` —
  passed after event-ID-bound signature and timestamp-tolerance changes.
- PostgreSQL-backed `TestPostgresRetryAndDeadLetter` — passed with the
  delivery-time URL rejection and dead-letter assertion.
- Current-revision independent-services test —
  `go test -p 1 -ldflags='-w' -count=1 -run
  '^TestProviderSandboxSeparateServices$' -v ./integration` — passed in
  283.496s (test runtime 279.43s) with local PostgreSQL and Temporal. Runtime
  output confirmed separate-service startup/restarts, provider status,
  customer credential/entitlement persistence, filtered trade and settlement
  reads, webhook configuration and signed delivery after Notification restart,
  callback replay, UNKNOWN recovery, callback-before-create ordering,
  funding/hold invariants, Settlement outbox delivery, and hold release after
  failure. After the fail-closed Trade Orchestrator guard was added, the
  independent-services test passed again in 283.496s.
- `go test -p 1 -ldflags=-w -count=1 ./...` with PostgreSQL enabled and
  Temporal unset — passed on the current revision after the signature change.
- `go test -p 1 -ldflags=-w -count=1 ./services/customer-kyb ./services/api-gateway`
  — passed after adding Customer/KYB credential lifecycle gRPC operations,
  environment persistence, route entitlements, and per-client/per-institution
  rate-limit enforcement. Verify verbose PostgreSQL test output before treating
  the database-backed restart case as exercised.
- `go vet -p 1 ./...` — passed on the current revision.
- `go test -p 1 -ldflags=-w -count=1 -timeout=5m ./...` — passed on this
  revision. PostgreSQL-dependent tests skip unless `OSAI_POSTGRES_DSN` is set;
  that variable was not configured in this environment.
- Focused Ledger tests cover FX evidence validation, independent per-currency
  journal balancing, rejection without explicit Finance-approved account
  mappings, and overflow-safe debit/credit totals. These are Ledger primitives
  only; durable FX execution posting and production integration remain
  unimplemented.
- `go test -p 1 -count=1 ./services/settlement/settlementcore ./workflows
  ./services/trade-orchestrator/...` — passed after hardening the legacy
  settlement callback path to persist evidence without accepting callback
  status as financial finality. Terminal results now require complete
  reference-bound provider-query evidence, durable runtime state recording,
  and the configured provider query before finalizing; missing evidence or
  recorder failure blocks the transition.
- PostgreSQL-backed `go test -p 1 -count=1 ./services/ledger/ledgerstore` —
  passed with local PostgreSQL. The test verifies a USD settlement command
  cannot consume or release an NGN reservation and creates no settlement
  journal.
- `go test -p 1 -ldflags=-w -count=1 -timeout=10m ./...` — all packages passed
  on 2026-10-12 with PostgreSQL enabled against a fresh isolated database and
  Temporal paused during the database-only suite (then restarted). The
  concurrent idempotency reservation test also passed 10 consecutive
  PostgreSQL-backed repetitions after fixing test-level environment leakage.
- `go vet -p 1 ./...` — passed after the current source changes.
- PostgreSQL-backed webhook replay test — passed after adding concurrent
  same-key requests, different-payload/key conflict, institution isolation,
  service restart, and durable attempt-history assertions.
These results do not satisfy the missing production, load, restore or public
contract gates above. The independent-services flow now exercises provider
status and trade/report filters, but the remaining FX, public credential
lifecycle, operational and production controls are still open.

## Required before a controlled pilot

1. Resolve and complete the remaining V1 wire contract from the Technical
   Handoff and OpenAPI: implement quote funding consistent with the
   user-defined total-cost formula; obtain Finance-approved account mappings;
   implement cross-currency quote calculation, verified FX evidence/accounting,
   recovery details, remaining quote fields, and error mappings. The latest
   independent-services run covers provider status, Settlement event
   publication, and all supported public list/report filters; production FX
   remains blocked pending approved mappings and a durable execution path.
2. Verify credential creation/rotation/revocation through independently
   running services and PostgreSQL restart; use shared rate-limit state if
   multiple gateway replicas are deployed.
3. Complete authoritative event publication and versioned payload schemas for
   FX-executed, failed/reversed, and customer-relevant reconciliation-break
   events; add customer delivery-history reads and operations workflows where
   required.
4. Verify route-level authorization against provisioned entitlements, finish
   service identity/private connectivity and secrets/key-rotation controls,
   and conduct a security review.
5. Wire the required metrics, dashboards, alert thresholds and corresponding
   tested runbooks.
6. Execute and record database backup/restore, balance rebuild, outbox/inbox
   replay, workflow recovery, load and failure-injection drills.
7. Extend independent-services acceptance coverage to the public list/report
   filters and rerun it with isolated Temporal state.

No production-ready or pilot-ready claim should be made until each blocking
gate has concrete evidence.
