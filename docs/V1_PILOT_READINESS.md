# V1 Pilot Readiness

Status: **NOT PILOT-READY** (assessment updated 2026-10-12). Passing unit and
database-backed tests do not certify production readiness. The current
independent-services acceptance test passed for the exercised customer flow
after the latest changes; it does not cover all public list/report filters or
the still-missing provider-status route or newly added credential lifecycle
routes.

## Gate assessment

| Gate | Verdict | Evidence / remaining work |
| --- | --- | --- |
| Ledger financial invariants | PASS for tested flows | Integer minor units, journal invariants, persisted ledger-derived customer reads, and PostgreSQL tests pass. Broader production integrity monitoring remains unproven. |
| Customer API contract | PARTIAL | `GET /v1/trades` and `GET /v1/reports/settlements` are implemented through owner gRPC APIs with filters, tenant scoping and pagination. Trade/report owner PostgreSQL tests and gateway tests pass. `GET /v1/providers/status` remains absent because entitlement naming and safe public provider-status fields are unresolved. The user-defined `total_cost_minor` formula and separate FX/payout lifecycle are recorded, but quote/trade/ledger do not implement source-currency funding plus fee or verified FX execution/accounting; do not expose total cost yet. |
| Authentication and authorization | PARTIAL | Customer/KYB persists sandbox/production credential scope, label, and hashed secret; public create/list/get/rotate/revoke routes use owner gRPC. Gateway enforces per-institution and per-credential token buckets and route entitlements. Owner unit/gRPC lifecycle and persistence/restart tests were added; whether the PostgreSQL test executed depends on `OSAI_POSTGRES_DSN`. Multi-gateway rate-limit aggregation, full service identity/private connectivity, independent-services credential flow, and broader security review remain outstanding. |
| API idempotency | PASS for covered mutations | Durable reservation, payload conflict detection, original response replay, and database-backed tests exist. Webhook replay additionally has owner-side durable idempotency and concurrent duplicate coverage. Failure/concurrency coverage across all public mutations still needs independent-services verification. |
| Quote/trade recovery | PASS for exercised flows | Owner-local persistence, settlement finality outbox and trade inbox are exercised by the current-revision independent-services proof, including restarts, replay and UNKNOWN recovery. Trade-list and settlement-report owner queries have PostgreSQL tests, but every filter is not exercised in the independent-services run. |
| Customer webhook security/delivery | PARTIAL | Signed delivery, encrypted configuration, durable retries/dead-letter, and restart recovery are implemented and unit/PostgreSQL tested. The public replay endpoint and durable replay history are implemented and tested for duplicate/concurrent keys, conflict, institution scope and store restart. The complete event catalog is not emitted; delivery-history read and operations workflows remain incomplete. |
| SSRF and provider URL controls | PARTIAL | Customer webhook requests reject non-public production targets and redirects. The wider provider URL allowlist and a full security assessment remain unproven. |
| Maker-checker and audit integrity | PARTIAL | Beneficiary maker-checker and append-only action records are covered. Other sensitive actions and hash-chained audit anchoring are not proven. |
| Metrics, alerts, dashboards and runbooks | FAIL | Required metrics/alerts are not fully wired, and required incident runbooks have not been exercised end-to-end. |
| Backup, restore and rebuild | FAIL | No qualifying database restore, balance rebuild, event replay, or reporting reconstruction drill is recorded. |
| Load and failure testing | FAIL | No complete quote burst, callback burst, DB exhaustion, slow dependency, Kafka/Temporal outage or instance-termination matrix has passed. |
| Independent services acceptance | PASS for exercised flow; remaining API scope unproven | Current-revision run passed in 139.36s with PostgreSQL and Temporal. It exercised independently running services, restart/replay, UNKNOWN recovery, callback ordering, funding/hold invariants, and hold release. It does not prove every new trade/report filter or absent provider-status and credential lifecycle routes. |

## Recent local test evidence

- `go test -p 1 -ldflags=-w -count=1 ./services/notification-webhook/...` —
  passed after event-ID-bound signature and timestamp-tolerance changes.
- PostgreSQL-backed `TestPostgresRetryAndDeadLetter` — passed with the
  delivery-time URL rejection and dead-letter assertion.
- Current-revision independent-services test —
  `go test -p 1 -ldflags='-w' -count=1 -run
  '^TestProviderSandboxSeparateServices$' -v ./integration` — passed in
  139.36s (test runtime 136.82s) with local PostgreSQL and Temporal. Runtime
  output confirmed service startup/restarts, webhook configuration and
  delivery, callback replay, UNKNOWN recovery, callback-before-create ordering,
  funding/hold invariants, and hold release after failure.
- `go test -p 1 -ldflags=-w -count=1 ./...` with PostgreSQL enabled and
  Temporal unset — passed on the current revision after the signature change.
- `go test -p 1 -ldflags=-w -count=1 ./services/customer-kyb ./services/api-gateway`
  — passed after adding Customer/KYB credential lifecycle gRPC operations,
  environment persistence, route entitlements, and per-client/per-institution
  rate-limit enforcement. Verify verbose PostgreSQL test output before treating
  the database-backed restart case as exercised.
- `go vet -p 1 ./...` — passed on the current revision.
- PostgreSQL-backed webhook replay test — passed after adding concurrent
  same-key requests, different-payload/key conflict, institution isolation,
  service restart, and durable attempt-history assertions.
These results do not satisfy the missing production, load, restore or public
contract gates above. The independent-services flow passed for its exercised
scope, but it does not exercise every implemented list/report filter or routes
and controls that have not yet been implemented.

## Required before a controlled pilot

1. Resolve and complete the remaining V1 wire contract from the Technical
   Handoff and OpenAPI: provider-status entitlement/public fields, quote
   funding implementation consistent with the user-defined total-cost
   formula, approved FX evidence/accounting and recovery details,
   cross-currency quote calculation, remaining quote fields and error mappings.
2. Verify credential creation/rotation/revocation through independently
   running services and PostgreSQL restart; use shared rate-limit state if
   multiple gateway replicas are deployed.
3. Complete authoritative event publication and versioned payload schemas for
   the required webhook event catalog; add customer delivery-history reads and
   operations workflows where required.
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
