# V1 Pilot Readiness

Status: **NOT PILOT-READY** (assessment updated 2026-10-12). Passing unit,
database-backed, and local independent-services tests do not certify
production readiness. The previously recorded independent-services run
passed its then-exercised customer flow, including provider status, Customer
restart, Settlement event publication, and filtered trade/report routes. The
expanded fee-bearing acceptance test has not passed on the current working
tree; its latest build failed before service startup because C: lacked disk
space. No policy approval is being reopened as an engineering prerequisite.
The hard NGN/USD route blocker is factual: no executed USD route/custody
agreement or independent USD custody evidence is present in the repository.
Executable cross-currency funding/FX, operational controls, and production
reliability gates remain open.

## Gate assessment

| Gate | Verdict | Evidence / remaining work |
| --- | --- | --- |
| Ledger financial invariants | PASS for tested flows | Integer minor units, journal invariants, persisted ledger-derived customer reads, and PostgreSQL tests pass. Broader production integrity monitoring remains unproven. |
| **5A — Quote and pricing correctness** | ENGINEERING PARTIAL; arithmetic, immutable propagation, and all-in reservation implemented; fee payout gated | Fixed-source/fixed-destination pricing, fee bounds, fixed-point rates, currency exponents, fee ceiling, conversion floor, and surplus tracking exist. The full immutable pricing snapshot now propagates from accepted quote into Trade and Ledger's all-in funding reservation. Trade and Ledger PostgreSQL tests verify persistence, restart reads, duplicate/concurrent replay, and altered-snapshot rejection. Fee-bearing trades remain gated before payout until Ledger can safely account for fee and principal. Independent-service verification of the extended fee path is still outstanding. |
| **5B — FX execution and payout lifecycle** | FAIL / blocked | Settlement has durable provider-query evidence/finality protections; a complete durable FX execution lifecycle, FX-to-payout recovery, and route-specific FX evidence do not exist. Unsupported cross-currency trades remain fail-closed. |
| **5C — Ledger and accounting** | PARTIAL; production FX posting blocked | Ledger provides exact rate/proceeds validation, balanced currency journal primitives, and explicit currency-matched account mappings. Durable FX posting/recovery, production route account IDs, and verified USD custody classification are absent. Do not infer an asset or enable inventory cost basis. Details in [FX_POSITION_AND_LEDGER_POLICY_REVIEW.md](./FX_POSITION_AND_LEDGER_POLICY_REVIEW.md). |
| **5D — API security and release gates** | PARTIAL | Covered authorization, idempotency, tenant-scoped reads, signed webhook delivery, and restart paths exist. Public credential lifecycle and all mutation failure/concurrency cases need independent-service evidence; operational and production gates remain open below. |
| Customer API contract | PARTIAL | `GET /v1/trades` and `GET /v1/reports/settlements` use owner gRPC APIs for tenant-scoped filtering and pagination; the independent-services test exercises supported filters. `GET /v1/providers/status` requires `providers:read`. Quote responses expose `total_cost_minor` as the all-in source-currency debit (equal to `total_debit_minor`) and `total_cost_currency` as `base_currency`. Accepted Trade reads now expose the immutable pricing snapshot. Fee-bearing quote acceptance and exact funding reservation have focused, PostgreSQL, restart, and concurrent replay coverage; the expanded independent-service acceptance has not completed. Fee-bearing payout and FX accounting remain incomplete. |
| Authentication and authorization | PARTIAL | Customer/KYB persists environment-scoped credentials, entitlements, and webhook configuration; the independent-services run proves a seeded credential remains usable after Customer restart. Public create/list/get/rotate/revoke routes use owner gRPC, but their complete lifecycle is not yet exercised through independent services. Gateway enforces per-institution and per-credential token buckets and route entitlements. Multi-gateway rate-limit aggregation, full service identity/private connectivity, and broader security review remain outstanding. |
| API idempotency | PASS for covered mutations | Durable reservation, payload conflict detection, original response replay, and database-backed tests exist. Webhook replay additionally has owner-side durable idempotency and concurrent duplicate coverage. Failure/concurrency coverage across all public mutations still needs independent-services verification. |
| Quote/trade recovery | PASS for exercised flows | Owner-local persistence, settlement finality outbox and trade inbox are exercised by the current-revision independent-services proof, including restarts, replay and UNKNOWN recovery. Trade-list and settlement-report queries are exercised independently with their supported filters and pagination. |
| Customer webhook security/delivery | PARTIAL | Signed delivery, encrypted configuration, durable retries/dead-letter, and restart recovery are implemented and unit/PostgreSQL tested. The public replay endpoint and durable replay history are implemented and tested for duplicate/concurrent keys, conflict, institution scope and store restart. Quote-expired, trade accepted/funding-required/funded, and Settlement-owned settlement-processing/successfully-settled events have owner-local transactional outboxes and relays; FX-executed, failed/reversed, and customer-relevant reconciliation-break publication remain blocked on authoritative state/evidence and tenant identity. Delivery-history read and operations workflows remain incomplete. |
| SSRF and provider URL controls | PARTIAL | Customer webhook requests reject non-public production targets and redirects. The wider provider URL allowlist and a full security assessment remain unproven. |
| Maker-checker and audit integrity | PARTIAL | Beneficiary maker-checker and append-only action records are covered. Other sensitive actions and hash-chained audit anchoring are not proven. |
| Metrics, alerts, dashboards and runbooks | FAIL | Required metrics/alerts are not fully wired, and required incident runbooks have not been exercised end-to-end. |
| Backup, restore and rebuild | FAIL | No qualifying database restore, balance rebuild, event replay, or reporting reconstruction drill is recorded. |
| Load and failure testing | FAIL | No complete quote burst, callback burst, DB exhaustion, slow dependency, Kafka/Temporal outage or instance-termination matrix has passed. |
| Independent services acceptance | PASS for previously exercised flow only; current expanded fee-flow unverified | The previously recorded run passed on 2026-10-12 in 279.43s with PostgreSQL and Temporal. It exercised provider status, Customer and Notification restarts, signed webhook retry, Settlement-owned events, trade and settlement filters, callback replay/order, UNKNOWN recovery, funding/hold invariants, and hold release. It predates the new fee-bearing acceptance assertions; the latest attempt to run the expanded test stopped during compilation because C: ran out of space. It does not prove quote funding with fees, cross-currency FX accounting, public credential create/rotate/revoke, or production controls. |

## NGN/USD route evidence snapshot

Repository search found no executed USD custody/provider agreement, USD
account statement, or production USD custody configuration. This is scoped to
files available in this repository, not a claim about documents held elsewhere.
The detailed evidence matrix and exact evidence needed are in
[FX_POSITION_AND_LEDGER_POLICY_REVIEW.md](./FX_POSITION_AND_LEDGER_POLICY_REVIEW.md).

| Item | Evidence found | Status and consequence |
| --- | --- | --- |
| Provider route | Flutterwave transfer adapter is NGN-only; the provider contract file is a pre-code draft. | Transfer integration verified; no executable NGN/USD FX contract is established. |
| USD ownership/custody | No executed account/custody agreement, independent USD statement, or configured USD account found. | Unverified; no production USD asset mapping/posting. Inventory costing only if Osai-owned fungible inventory is evidenced. |
| NGN prefunding | Local handoff documents a test-only prefund fixture and explicitly says it is not real bank-funding evidence. | Test mechanism only; does not authorize production prefunding or settlement. |
| Chart mapping | Ledger primitives check explicit mapping currency; tests use placeholders, no production chart IDs. | Primitives verified; no production FX journal mapping. |
| Simulation | Simulator uses a separate route identity; local proof funding is gated to test use. | Test behavior only; never evidence of actual provider/custodian funds or settlement. |

## Authoritative Phase 5 engineering checklist

This section is the single completion checklist. Status is based on code and
tests actually present; a passing unit test does not substitute for the stated
acceptance test. Route-specific production evidence is tracked separately
above.

| Deliverable | Status | Code/configuration | Executable acceptance evidence | Dependency / blocker |
| --- | --- | --- | --- | --- |
| 5A pricing arithmetic: modes, fees, exact rate/exponents, rounding/surplus | **Implemented; unit-covered** | `services/quote/quotecore/pricing.go` | `go test -p 1 -count=1 ./services/quote/quotecore` (fixed-source, fixed-destination, fees, exponent conversion, overflow, rounding) | None for quote arithmetic. |
| 5A public quote snapshot, expiry, idempotent replay | **Implemented; full snapshot persists through Trade and Ledger reservation** | `services/quote/quotecore/quotes.go`, `services/quote/grpc_server.go`, `services/trade-orchestrator/tradecore/`, `proto/osai/ledger/v1/ledger.proto`, `services/ledger/ledgerstore/funding.go`, `pkg/pricing/snapshot.go` | Focused pricing/Quote/Trade/Gateway tests passed; PostgreSQL-backed Ledger and Trade tests passed, including reservation economics, persisted snapshot read, restart, concurrent replay, and altered-snapshot conflict. | Independent-service end-to-end verification of the fee-bearing path still required. |
| 5A `total_cost_minor` contract | **Implemented in quote response; latest focused test passed** | `proto/osai/quote/v1/quote.proto`, `services/quote/grpc_server.go`, `services/api-gateway/grpc_clients.go`, `services/api-gateway/gateway.go`, `api/openapi/osai-v1.yaml` | `go test -p 1 -count=1 ./services/quote ./services/api-gateway`; assert it equals `total_debit_minor` and currency equals source | Generated protobuf binding updated. It does not imply a completed fee-bearing funding flow. |
| 5A quote-to-Trade/Ledger funding with fee | **Partial; exact all-in reservation and immutable snapshot persistence implemented; fee-bearing payout fail-closed** | `pkg/pricing/snapshot.go`, `services/quote/grpc_server.go`, `services/trade-orchestrator/grpc_server.go`, `services/ledger/ledgergrpc/server.go`, `services/ledger/ledgerstore/funding.go`, `integration/provider_sandbox_test.go` | `go test -p 1 -ldflags=-w -count=1 ./services/trade-orchestrator/...` passed with PostgreSQL enabled; `go test -p 1 -ldflags=-w -count=1 ./services/ledger/...` passed with PostgreSQL enabled. The focused Ledger reservation test persists and rereads the complete snapshot and covers concurrent replay/conflict; the Trade PostgreSQL test covers restart, concurrent replay, and altered snapshot rejection. Expanded independent-service build failed before assertions with `There is not enough space on the disk`. | Independent service confirmation remains outstanding. Fee accounting/posting and payout are intentionally gated. |
| 5B durable FX execution separate from payout | **Not implemented end-to-end** | Settlement evidence path exists; no FX execution store/service/outbox | Add DB integration tests for duplicate/concurrent execution commands, unknown query-first recovery, restart, source/destination evidence mismatch, and payout-failure recovery | FX executor and settlement obligations not established for production route; simulator can only test non-production lifecycle. |
| 5C durable FX journals/recovery and configurable mappings | **Primitives only** | `services/ledger/ledgercore/fx.go`; funding storage under `services/ledger/ledgerstore/` | Add PostgreSQL-backed journal idempotency, per-currency mapping/ownership validation, evidence conflict, restart, reconciliation and recovery tests | Production USD custody/ownership and chart mapping are absent; production FX posting remains disabled. |
| 5D API security/idempotency/webhooks/tenant isolation | **Partial; exercised paths pass** | Gateway, Customer/KYB, Notification, owner service stores | Unit + PostgreSQL mutation concurrency/conflict tests; independent `TestProviderSandboxSeparateServices` for restart/isolation | Credential lifecycle, full public mutation matrix, security review, operational controls remain unproven. |
| 5D operational release controls: alerting, restore, load/failure drills | **Not complete** | Deployment, migrations, runbooks, observability | Record restore/rebuild, replay, load and dependency/termination failure runs; verify alerts and runbooks | No completed evidence recorded. |
| NGN/USD production route evidence | **Blocked; repository search evidence recorded above** | Provider configuration, executed route/custody agreements, statements, account chart | Verify executed route agreement + independent NGN/USD custody/settlement evidence and actual account IDs | External route evidence not present in repository; no production FX readiness claim. |

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
- Focused Ledger tests cover FX evidence validation (including exact applied
  rate/proceeds consistency, currency exponents, and fee-currency scope),
  independent per-currency journal balancing, rejection without explicit or
  currency-matched account mappings, and overflow-safe debit/credit totals.
  These are Ledger primitives only; durable FX execution posting and
  production integration remain unimplemented. The repository contains no
  executed USD provider/custody agreement or independent USD account evidence,
  so the USD asset classification and any inventory cost-basis policy remain
  unresolved and production FX stays disabled.
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
- Latest working-tree verification: `go test -p 1 -ldflags=-w -count=1
  -timeout=10m ./...` passed and `go vet -p 1 ./...` passed. The full-suite
  command ran without `OSAI_POSTGRES_DSN`, so its opt-in PostgreSQL cases are
  not evidence of a database-backed full-suite run.
- Current-revision independent-services acceptance:
  `OSAI_POSTGRES_DSN=postgres://osai:...@127.0.0.1:5433/osai?sslmode=disable
  OSAI_TEMPORAL_ADDR=127.0.0.1:7233 go test -p 1 -ldflags=-w -count=1
  -timeout=10m -run '^TestProviderSandboxSeparateServices$' -v ./integration`
  passed (`ok .../integration 164.585s`; test body 160.30s). It provisioned and
  removed a fresh PostgreSQL database, started independent service processes,
  and exercised provider status, Customer and Notification restarts, signed
  webhook retry, callback replay/order, UNKNOWN recovery, funding/hold
  invariants, settlement outbox delivery, and hold release after failure.
- The quote/gateway contract tests passed in the full-suite run. They verify
  `total_cost_minor == total_debit_minor` and source-currency reporting. The
  pricing regression tests prove percentage-fee ceiling, conversion floor, and
  fixed-destination minimum principal. These do not prove fee-bearing quote
  funding or cross-currency FX execution.
- Latest Stage 1 tests passed:
  `go test -p 1 -ldflags=-w -count=1 ./services/trade-orchestrator/...` and
  `go test -p 1 -ldflags=-w -count=1 ./services/ledger/...`, both with local
  PostgreSQL enabled. The Trade PostgreSQL test verifies snapshot restart,
  concurrent same-quote replay, and altered-snapshot rejection. The Ledger
  PostgreSQL test verifies all-in reservation, snapshot persistence/readback,
  concurrent idempotent reservation, and conflicting snapshot rejection.
- The expanded `TestProviderSandboxSeparateServices` now includes a fee-bearing
  quote path, insufficient-funding retry, exact all-in reserve, and payout
  gating assertions. Its latest build attempt stopped before service startup
  and assertions because Go could not write build artifacts due to
  insufficient C: disk space. The previously recorded independent-services
  pass predates those new assertions and does not verify them.
These results do not satisfy the missing production, load, restore or public
contract gates above. The independent-services flow now exercises provider
status and trade/report filters, but the remaining FX, public credential
lifecycle, operational and production controls are still open.

## Remaining implementation and release gates

1. Complete 5A quote-to-Trade/Ledger propagation for principal, fee, total
   debit, destination proceeds/surplus, mode, rate, precision, and expiry.
   The quote response now defines `total_cost_minor` as the source-currency
   all-in debit; fee-bearing funding and Ledger posting remain fail-closed.
2. Complete 5B durable FX execution separate from payout, including
   route-specific executor evidence, owner-local outbox/deduplication,
   query-first UNKNOWN recovery, reconciliation, and failed/returned payout
   recovery.
3. Complete 5C durable Ledger posting and recovery, reporting consistency, and
   independently balanced currency journals. Keep the affected production FX
   postings disabled until route/custody evidence establishes ownership,
   settlement obligations, and actual account mapping.
4. Verify credential creation/rotation/revocation through independently
   running services and PostgreSQL restart; use shared rate-limit state if
   multiple gateway replicas are deployed.
5. Complete authoritative event publication and versioned payload schemas for
   FX-executed, failed/reversed, and customer-relevant reconciliation-break
   events; add customer delivery-history reads and operations workflows where
   required.
6. Verify route-level authorization against provisioned entitlements, finish
   service identity/private connectivity and secrets/key-rotation controls,
   and conduct a security review.
7. Wire the required metrics, dashboards, alert thresholds and corresponding
   tested runbooks.
8. Execute and record database backup/restore, balance rebuild, outbox/inbox
   replay, workflow recovery, load and failure-injection drills.
9. Extend independent-services acceptance to the full fee-bearing quote/trade
   flow, public credential lifecycle, and supported list/report filters, with
   isolated services, PostgreSQL, and Temporal.

No production-ready or pilot-ready claim should be made until each blocking
gate has concrete evidence. These are engineering and verification gates, not
requests to reopen policy approvals; use the conservative defaults in the FX
policy review and keep only unsupported route-specific real-money behavior
disabled.
