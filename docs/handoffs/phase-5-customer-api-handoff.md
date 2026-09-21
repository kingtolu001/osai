# Phase 5 Handoff — Customer API

## Objective

Phase 5 scopes Osai's external customer API as a stable, partner-facing product surface that hides internal microservice complexity while preserving the architecture's ownership boundaries.

This is not a rewrite of the ledger or trade domain. Public-facing customer access is intentionally routed through the API Gateway, which translates HTTP/V1 requests into typed internal gRPC commands and queries. The authoritative implementation remains in the domain services behind the gateway.

## Implementation status

This repo now contains the first working Phase 5 foundation for the customer ownership path and the public customer-facing edge:

- institutional customer and API credential model in the customer-KYB service boundary
- authenticated API Gateway middleware with request-scoped correlation IDs and institution checks
- public quote creation and quote acceptance endpoint handling with idempotency-key replay protection
- hashed credential storage and secret material returned only at creation time
- outbound webhook signing and verification utilities for external customer delivery
- focused tests covering customer credential validation, gateway idempotency behavior, and outbound signature verification

This is intentionally conservative and remains a narrow V1 baseline that respects the ledger as sole financial truth. It is not a production-complete customer API surface covering every partner flow or every future feature. The architecture remains: gateway owns edge concerns only; customer-KYB owns institution state; quote and trade services remain authoritative for lifecycle state; ledger is still the sole financial journal authority.

## Authoritative design inputs reviewed

The following repo sources were treated as the contract floor before drafting this handoff:

- [docs/SERVICE_OWNERSHIP.md](../SERVICE_OWNERSHIP.md)
- [docs/IMPLEMENTATION_MANIFEST.md](../IMPLEMENTATION_MANIFEST.md)
- [docs/GRPC_CONVENTIONS.md](../GRPC_CONVENTIONS.md)
- [docs/EVENT_CONVENTIONS.md](../EVENT_CONVENTIONS.md)
- [docs/adr/ADR-microservices-from-day-one.md](../adr/ADR-microservices-from-day-one.md)
- [osai_security_observability_design.md](../../osai_security_observability_design.md)
- [osai_state_machines_design.md](../../osai_state_machines_design.md)
- [osai_idempotency_eventing_design (1).md](../../osai_idempotency_eventing_design%20(1).md)
- [osai_ledger_money_model_v0.2.md](../../osai_ledger_money_model_v0.2.md)
- [docs/handoffs/sprint-0-handoff.md](sprint-0-handoff.md)
- [docs/handoffs/phase-2-quote-sandbox-handoff.md](phase-2-quote-sandbox-handoff.md)
- [docs/handoffs/phase-3-first-provider-pair-handoff.md](phase-3-first-provider-pair-handoff.md)
- [docs/handoffs/phase-4-reconciliation-handoff.md](phase-4-reconciliation-handoff.md)

This handoff does not invent public API behavior where the repo's existing architecture explicitly defines only the service boundaries and invariants. Where the repo lacks a final API ADR, the V1 public surface stays intentionally minimal and conservative.

## Service boundaries and ownership

### api-gateway

The API Gateway owns the customer-facing HTTP surface and policy enforcement.

- Public HTTP/V1 only; no service database ownership.
- Converts HTTP requests to typed internal gRPC calls.
- Enforces auth, tenant scoping, idempotency, correlation propagation, rate limiting, and error mapping.
- Must not expose direct microservice database rows or raw internal error payloads.

### customer-kyb-service

The customer-KYB service owns customer, institution, and compliance metadata needed for V1.

- institution profile and status
- KYB/compliance state
- accounts
- permissions
- approved beneficiaries
- limits and tier hooks

It does not invent a consumer onboarding product or a separate public portal lifecycle. It manages the institution-scoped operational records required to allow or deny money movement and beneficiary flows.

### notification-webhook-service

The notification-webhook service owns outbound signed webhooks to customer systems.

- event dispatch to external endpoints
- signature generation and timestamping
- replay protection guidance, retry/backoff, and delivery history
- no direct access to money-posting tables

## Authentication and authorization

### Production-oriented client credentials

The repo security design requires client credentials that are production-oriented and environment-scoped.

Required properties:

- API key / client credentials model for customer API consumers
- environment scoping (sandbox / staging / production) per credential
- revocation and rotation support with no downtime window for valid clients during migration
- rate limiting per client ID and tenant/institution
- correlation ID propagation from ingress to downstream domain calls
- no logging of secrets, credentials, or signing keys

The security design also states:

- client API keys must be stored hashed; secrets are shown once at creation
- service-to-service calls must use workload identity or short-lived credentials rather than static secrets
- ops access remains separate and not part of the customer public surface

### Authorization model

The repo security ADR defines the operating RBAC pattern. For V1 customer-facing APIs, the effective policy should be constrained to the institution scope:

- viewer: read-only own institution data
- trader: create quotes and manage own beneficiaries within institution scope
- ops: limited operational visibility as allowed by implementation policy
- admin: credential, institution-scoped entitlement management only where explicitly authorized

Any action that can move money or change a payout destination must be maker-checker controlled at the relevant domain boundary. The public API can enforce the client-side authorization and pass the caller identity through the correlation chain, but must not bypass the service's own domain validation.

## Public V1 API surface

The repo does not contain a final partner-facing REST contract doc beyond the architectural minimum and design requirements. Therefore, the public API should be kept intentionally minimal and aligned to the already-defined domain boundaries.

Minimum architecture-equivalent public surface for V1:

- POST /v1/quotes
- GET /v1/quotes/{id}
- POST /v1/quotes/{id}/accept
- GET /v1/trades/{id}
- GET /v1/trades
- POST /v1/beneficiaries
- GET /v1/balances
- GET /v1/transactions
- GET /v1/reports/settlements
- POST /v1/webhook-endpoints
- GET /v1/providers/status where entitled

Additional notes:

- Public HTTP traffic is an ingress boundary only; API Gateway translates those HTTP requests into internal gRPC requests.
- No API handler may read or write the domain service database directly.
- Public outputs must be read-model or workflow-derived data, not a second financial truth source.
- Customer-visible balances derive from the ledger service, and trade state derives from the canonical trade workflow.

## Idempotency requirements

The architecture requires documented money/state-creating operations to carry an idempotency key, and the same key must return the original result for replay.

Required behavior

- every state-creating or money-facing endpoint accepts an idempotency key
- same key + same payload => returns original result
- same key + different payload => reject with an explicit idempotency conflict error
- result is stored and returned, never re-created or recomputed under a new effect

This is consistent with the repo's idempotency design and with the known state-machine and ledger invariants:

- no silent double posting
- no second journal effect for the same client command
- replay is safe and is treated as a duplicate, not a new business action

## Error model

The public API must use stable, machine-readable error codes plus the required correlation ID behavior.

Required pattern:

- stable error code per domain condition
- correlation ID included in every error response
- safe human-facing message, no internal stack traces
- no raw provider data leakage
- no internal service names or database details in customer-facing errors

The repo's gRPC conventions define the standard semantics for the internal service layer:

- INVALID_ARGUMENT
- NOT_FOUND
- ALREADY_EXISTS
- FAILED_PRECONDITION
- UNAUTHENTICATED
- PERMISSION_DENIED
- RESOURCE_EXHAUSTED
- UNAVAILABLE
- INTERNAL

The public HTTP layer should translate these to stable API error codes and safe messages without exposing internal implementation details.

## Customer webhook behaviour

The repo security design requires outbound signed webhooks and the event conventions require event envelope consistency.

Required webhook contract:

- stable event ID for deduplication
- HMAC-SHA256 signature using a shared endpoint secret
- timestamp header
- at-least-once delivery semantics
- exponential retry with backoff and delivery history recording
- dead-letter handling for repeated failure
- replay protection guidance using event IDs

Required envelope fields:

- event_id
- aggregate_type
- aggregate_id
- sequence
- event_type
- correlation_id
- occurred_at
- payload

The webhook service is not the source of financial truth; it is an at-least-once delivery system for facts already created in the domain services.

## Read models and financial truth

The architecture explicitly forbids creating competing financial truth in API read models.

Therefore:

- customer-visible balances derive from the ledger service
- trade state derives from the canonical trade workflow
- read models must be derived, never authoritative
- the public API can expose a filtered or normalized projection of ledger/trade state, but not a second writable source of financial truth

## Recommended documentation set

Before Phase 6 implementation, the public API surface should ship with the following docs:

- OpenAPI specification
- authentication and client-credentials guide
- idempotency guide
- error catalogue
- webhook guide with signing requirements and replay guidance
- example integration flow
- sandbox instructions

The repo already defines security, observability, idempotency, and event rules; the public docs should reflect those rather than inventing new product patterns.

## Test plan

The Phase 5 work should be validated with the following tests:

- authentication and credentials validation
- authorization and tenant isolation
- rate limiting and quotas
- idempotency and replay correctness
- quote flow and trade retrieval
- balance correctness from ledger truth
- beneficiary authorization and approval gating
- webhook signing and verification
- webhook retry and replay behaviour
- error mapping and safe-error leakage checks
- provider-data leakage prevention
- pagination and filtering behaviour

Tests should validate actual domain behaviour and real API responses; they should not assert mock-only behaviour.

## Known integration gaps and unresolved design items

The repo does not yet include a final V1 public API specification document or a provider-specific customer onboarding contract. As a result, the following areas remain open before a production partner integration can be signed off:

- exact request/response schema for every public endpoint
- final customer tenant and institution entitlement model
- final partner account / beneficiary lifecycle rules
- exact provider status semantics and access entitlement matrix
- final webhook endpoint registration and revocation mechanics
- exact filter and pagination contract for balances, transactions, and reports
- exact customer-facing error catalog values for all public endpoints
- any additional institution compliance states beyond the design placeholders already present in the repo

These items are not a reason to invent speculative API behaviour. The correct Phase 5 baseline is to keep the V1 public contract narrow, explicit, and architecture-consistent.

## Final Phase 5 gate matrix

| Gate | Status |
| --- | --- |
| 1 | FAIL |
| 2 | FAIL |
| 3 | FAIL |
| 4 | FAIL |
| 5 | PASS |
| 6 | PASS |
| 7 | FAIL |
| 8 | PASS |
| 9 | PASS |
| 10 | PASS |
| 11 | PASS |
| 12 | PASS |
| 13 | PASS |
| 14 | PASS |
| 15 | PASS |
| 16 | PASS |
| 17 | FAIL |
| 18 | FAIL |
| 19 | FAIL |

### Evidence notes

- Gate 1 FAIL: the historical HTTP reporting responses were successful, but source inspection shows their balances and transactions are hard-coded seeds in reporting/main.go, not a verified ledger-derived projection. HTTP success does not prove financial truth.
- Gates 5 and 6 PASS for the historical live HTTP quote replay and acceptance evidence below. These do not establish PostgreSQL durability or restart safety.
- Gate 7 FAIL: the current source audit finds A = 5, B = 0, C = 0, D = 4, E = 0, F = 1 under the explicitly defined counting scope below. The earlier all-zero claim is superseded.
- Gate 8 PASS: Redocly validation succeeded with 17 warnings only; no syntax or schema errors were reported.
- Gate 9 PASS: go mod tidy completed successfully.
- Gate 10 PASS: go test ./... completed successfully.
- Gate 11 PASS: go vet ./... completed successfully.
- Gate 12 PASS: all eight requested service race batches completed with exit code 0 on 2026-09-21. See coverage limitations below.
- Gates 2, 3, 4, 17, 18, 19 remain FAIL: live webhook success/retry, SQL durability, and final end-to-end closure are not established. The pre-existing handoff does not define individual acceptance criteria for all of these gate numbers; no new meanings are invented here. Gates 13-16 retain the prior recorded PASS status, not a new independent certification. OTel remains PASS based on the supplied trace evidence.

## Final status

Phase 5 remains incomplete. OTel remains PASS and race validation is complete. Live outbound webhook delivery, automatic retry, PostgreSQL durability for the customer flow, and architecture compliance are FAIL. The current checkout needs implementation work before those runtime proofs can succeed. No Phase 6 work was started.

## Phase 6 blockers

The following items should be treated as Phase 6 blockers for any production partner rollout:

- final OpenAPI contract and version freeze for V1
- exact institution onboarding and entitlement model with operational approvals
- final webhook registration and replay API/ops workflow
- final customer auth lifecycle, including credential rotation and revocation flows
- final error catalog and API compatibility policy
- final provider status gating and entitlement model for privileged endpoints
- operational runbooks for auth incidents, rate-limit events, and webhook backlog/replay containment

## Exit criterion

The Phase 5 exit criterion is not an unbounded product surface. It is a design-partner-ready customer API that can be safely used without exposing internal microservice complexity, while respecting the repo's ownership, security, idempotency, ledger, and webhook invariants.

A design partner can integrate using the documented public APIs if the public contract remains minimal, stable, and aligned to the internal architecture. Anything broader should wait until the missing API/entitlement details are authoritatively defined.

## Live runtime evidence (2026-09-20)

The verified runtime baseline as of this date is narrower than the full Phase 5 target and should be treated as the current supported state.

### Gate 5 — live quote idempotency

Validated against the running separate-process topology:

- API Gateway on :8082
- customer-KYB gRPC on localhost:50052
- quote gRPC on localhost:50053

Commands executed:

```powershell
$headers = @{
  'Authorization' = "ApiKey $($env:OSAI_SANDBOX_CLIENT_ID):$($env:OSAI_SANDBOX_CLIENT_SECRET)"
  'Content-Type' = 'application/json'
  'Idempotency-Key' = 'idem-live-g5-1'
}
Invoke-WebRequest -Uri 'http://localhost:8082/v1/quotes' -Method Post -Headers $headers -Body '{"base_amount_minor":10000,"base_currency":"NGN","quote_currency":"USD","destination_rail":"sim_lp_1","urgency":"normal"}' -UseBasicParsing
```

Observed results:

- first call: HTTP 200, quote_id = quo_048644292fdb, status = QUOTED
- same institution + same key + identical payload: HTTP 200, same quote_id = quo_048644292fdb
- same institution + same key + modified payload: HTTP 409, error code = IDEMPOTENCY_CONFLICT

This confirms the customer API gateway idempotency behavior at the HTTP layer is working as implemented.

### Gate 6 — live quote acceptance

Verified live acceptance result for the separate-process runtime topology:

- customer/KYB gRPC: 50052
- quote gRPC: 50053
- trade gRPC: 50054
- settlement gRPC: 50055
- API gateway HTTP: 8082

Fresh service PIDs observed during the final proof:

- customer/KYB: 23576
- quote: 12868
- trade-orchestrator: 10320
- settlement: 24020
- API gateway: 9308

Live quote created successfully:

- quote_id = `quo_1e90123ad2e0`
- request_id = `req_be5865a7-5e82-4063-bc07-c38ea461ce47`
- status = `QUOTED`

Live acceptance result:

- HTTP status = `200`
- quote_id = `quo_1e90123ad2e0`
- trade_id = `trd_ebbd680f-b347-456d-a42c-7bc6600017bf`
- status = `ACCEPTED`

Live downstream evidence from the service logs:

- `quote accepted: quote_id=quo_1e90123ad2e0 trade_id=trd_ebbd680f-b347-456d-a42c-7bc6600017bf ...`
- `trade created: trade_id=trd_ebbd680f-b347-456d-a42c-7bc6600017bf ... settlement_id=si_b787bcff-f901-40ef-93fc-c8cfb0dbf22c`
- `settlement instruction created: settlement_id=si_b787bcff-f901-40ef-93fc-c8cfb0dbf22c trade_id=trd_ebbd680f-b347-456d-a42c-7bc6600017bf ...`

Settlement ID retrieval:

- public accept response intentionally does not expose settlement_id; it only exposes `quote_id`, `trade_id`, `status`, and `request_id`.
- the settlement ID was retrieved directly via the live settlement gRPC boundary using the trade's settlement reference.

Replay proof:

- same acceptance Idempotency-Key + same body -> HTTP `200`
- same trade_id = `trd_ebbd680f-b347-456d-a42c-7bc6600017bf`
- same settlement_id = `si_b787bcff-f901-40ef-93fc-c8cfb0dbf22c`
- no second trade or second settlement instruction was created

Conflict proof:

- same Idempotency-Key + changed body -> HTTP `409`
- response body contains `IDEMPOTENCY_CONFLICT`

Validation proof:

```powershell
cd C:\Users\toluk\Desktop\osai; go test ./services/api-gateway/...; go test ./services/quote/...; go test ./services/trade-orchestrator/...; go test ./services/settlement/...; go test ./...; go vet ./...
```

Observed result:

- exit code = `0`
- all targeted validation passed

## Sign-off status

This Phase 5 handoff is now updated to the verified live runtime state for Gate 6. The architecture is proven as a separate-process chain:

Client -> API Gateway -> Quote gRPC -> Trade gRPC -> Settlement gRPC

The final Gate 6 evidence is complete and passes the condition that a quote is accepted, resulting in exactly one trade and exactly one settlement instruction in the live runtime topology.

## OTel correlation evidence (2026-09-21)

The final live OTel proof ran against the separate-process topology with the local OTel collector, using correlation ID `req-otel-phase5-final-v6`.

Business result:

- quote request: HTTP `200`, quote_id = `quo_068a0f23d3f3`, status = `QUOTED`
- acceptance request: HTTP `200`, trade_id = `trd_c0ce2556-b68a-4f2e-a418-788244cbbd6c`, status = `ACCEPTED`
- service logs recorded one settlement instruction for the accepted trade

Collector result:

- shared trace ID: `3c673f7277bc634c80e6cf0d3528f20c`
- API Gateway -> Quote client span: span ID `534071622e7057bc`, parent ID ``, operation `osai.quote.v1.QuoteService/AcceptQuote`
- Quote server span: span ID `f9784ab87975c7df`, parent ID `534071622e7057bc`, operation `osai.quote.v1.QuoteService/AcceptQuote`
- Quote -> Trade client span: span ID `f487db7c27bf4c4f`, parent ID `f9784ab87975c7df`, operation `osai.trade.v1.TradeService/CreateTradeFromAcceptedQuote`
- Trade server span: span ID `1819089050bbf7ce`, parent ID `f487db7c27bf4c4f`, operation `osai.trade.v1.TradeService/CreateTradeFromAcceptedQuote`
- Trade -> Settlement client span: span ID `b31dd4922d0297a4`, parent ID `1819089050bbf7ce`, operation `osai.settlement.v1.SettlementService/CreateSettlementForTrade`
- Settlement server span: span ID `cd3edea42a38565d`, parent ID `b31dd4922d0297a4`, operation `osai.settlement.v1.SettlementService/CreateSettlementForTrade`

The trace IDs are continuous across the synchronous API Gateway -> Quote -> Trade -> Settlement chain, each service has a distinct span, and the exported attributes contain RPC operation metadata without credential material or secret-bearing headers.

## Phase 5 closure verification (2026-09-21)

This section supersedes earlier closure claims where they conflict. OTel was not reopened or rerun. Its accepted trace remains `3c673f7277bc634c80e6cf0d3528f20c`, quote `quo_068a0f23d3f3`, trade `trd_c0ce2556-b68a-4f2e-a418-788244cbbd6c`.

### Live customer webhook and retry: FAIL

The required live proof cannot run through the current Notification/Webhook process:

- `services/notification-webhook/main.go` only wires provider ingress to Temporal. It has no customer event consumer, outbox relay, dispatcher, or retry worker.
- `services/customer-kyb/grpc_server.go:GetWebhookConfiguration` returns a fixed `https://example.invalid/webhook`; it does not retrieve a persisted institution endpoint/subscription/secret configuration.
- `webhookcore/customer_outbound.go` contains an HTTP signing helper and map-backed configuration/event/attempt storage. No production caller invokes `DeliverCustomerEvent`, `EnqueueEvent`, or `RecordAttempt`.
- Every helper call returns attempt number 1. `RecordAttempt` does not increment it or change the event status. Enqueue overwrites an existing event ID. Backoff is linear and no scheduler consumes it. No terminal/dead-letter worker exists.

Consequently, event_id, event_type, correlation_id, HTTP status, attempt_number and final persisted SUCCESS for a live customer event are **not available**. No local receiver was started because the required producer-to-dispatcher path does not exist. No customer configuration was fabricated through SQL. The existing `tmp_webhook_retry_proof.go` manually invokes the helper twice with a constructed event and an in-memory store; it is not qualifying live or durable retry evidence. Signing helper tests pass, but do not close these gates. No webhook secret was generated or exposed during this verification.

### PostgreSQL durability: FAIL

Read-only verification ran against healthy `osai-postgres`, database `osai`, at `2026-09-21 10:58:41 UTC`. Reproducible queries are in `deploy/local/phase5-durability-verification.sql`; output is in `logs/phase5-closure/sql-durability.txt`. The SQL uses a read-only transaction and excludes credential material and payload bodies.

| Owner / required evidence | Observed result |
| --- | --- |
| Customer/KYB institution and credential hash | No corresponding tables; service stores institutions and hashed credentials in maps. Hashing in memory is not persistence. |
| Gateway idempotency | Table exists; 3 quote records for `inst_sandbox_local`. No rows reference the supplied OTel quote or trade IDs. |
| Quote | No quote table; runtime creates `quotecore.NewQuoteStore()`. |
| Trade | No trade table; runtime creates `tradecore.NewStore()`. |
| Settlement | 4 rows exist, but runtime creates `settlementcore.NewStore(nil)` rather than using the available PostgreSQL adapter. Table lacks institution_id, trade_id and quote_id; these rows cannot prove ownership or this live flow. |
| Reporting | No projection table; runtime seeds balances and transactions in memory without deriving them from Ledger. |
| Notification | No config, outbound-event, delivery-attempt or outbox tables. |

Inventory contained 12 public tables: gateway idempotency, 2 ledger, 6 reconciliation and 3 settlement tables. There were no other application schemas. Existing test-created records are not evidence of the customer runtime chain. One quote/trade/settlement after replay, one logical webhook event with multiple attempts, and persisted ownership across the chain remain unproven. No cross-service runtime SQL was added.

### Architecture audit: FAIL

Counting scope: non-test Go source under `services/`, with `cmd/` and `workflows/` reviewed for database and ledger bypasses. Service-owned launcher/worker imports are not cross-service imports. A counts direct import declarations (not transitive dependencies); D counts substitute implementations compiled in non-test service source, including constructors unused by the current main; F counts gateway-owned financial domain state types, excluding edge idempotency responses.

| Category | Exact count | Evidence |
| --- | ---: | --- |
| A. Cross-service implementation/store imports | 5 | Gateway -> quotecore in gateway.go and grpc_clients.go; router -> quotecore in routingcore/router.go; trade -> routingcore and quotecore in tradecore/simulated_trade.go. Four distinct dependency edges. |
| B. Cross-service private DB access | 0 | SQL adapters access their own service tables. |
| C. Cross-service SQL joins | 0 | The ledger entries/journals join stays within Ledger ownership. |
| D. Fake/in-memory replacements for required remote boundaries | 4 | Gateway customerServiceClientAdapter, quoteServiceClientAdapter, defaultReadModelClient; Reporting seeded balanceStore replacing ledger-derived data. The first three are compiled helper implementations; Reporting's seed is active in main. |
| E. Direct ledger writes outside Ledger Service | 0 | Journal/entry inserts are in ledgerstore; the Temporal worker uses the Ledger gRPC client. |
| F. Gateway-owned financial state | 1 | Gateway.quoteStore / NewGateway owns and mutates QuoteStore. Current main uses remote clients, but the implementation remains in non-test gateway source. |

Import occurrences are saved in `logs/phase5-closure/cross-service-imports.txt`. Separately, gateway idempotency silently falls back to maps on database initialization errors and ignores persistence failure in SaveResponse. Customer, quote, trade, settlement and notification map stores are additional durability blockers, not counted as remote-boundary substitutes in D.

### Race batches and final validation

All commands below completed with exit code 0. Race commands ran sequentially to avoid concurrent linker pressure. Logs are under `logs/phase5-closure/`.

| Command | Result | Coverage / log |
| --- | --- | --- |
| go test -race ./services/api-gateway/... | PASS | race-api-gateway.txt |
| go test -race ./services/customer-kyb/... | PASS | race-customer-kyb.txt |
| go test -race ./services/quote/... | PASS | No test files; build only. race-quote.txt |
| go test -race ./services/trade-orchestrator/... | PASS | tradecore tests; main has no tests. race-trade-orchestrator.txt |
| go test -race ./services/settlement/... | PASS | Core/store tests; main and ledgerclient have no tests. race-settlement.txt |
| go test -race ./services/reporting/... | PASS | No test files; build only. race-reporting.txt |
| go test -race ./services/notification-webhook/... | PASS | webhookcore tests; main has no tests. race-notification-webhook.txt |
| go test -race ./services/ledger/... | PASS | API/core/gRPC/store tests; main has no tests. race-ledger.txt |
| go mod tidy | PASS | go-mod-tidy.txt; dependency classification/checksums updated for existing imports. |
| go test ./... | PASS | go-test.txt |
| go vet ./... | PASS | go-vet.txt |
| npx.cmd @redocly/cli lint api/openapi/osai-v1.yaml | PASS | Exit 0, 17 warnings; openapi.txt. |

Some Go results were cached. Passing existing tests does not establish untested runtime durability or delivery. No linker-memory failure or race assertion occurred in these batches. Initial tidy/test/npm invocations encountered sandbox cache-access failures; authorized retries succeeded. These were environment failures, not code failures. Windows `npx.cmd` is used for the requested npx command.

### Sign-off decision

Phase 5 closure is **FAIL**. Webhook success, retry, SQL durability and architecture do not meet the required gates. OTel remains PASS and requested race/validation commands are complete. The three success-only sign-off statements are withheld. Closing the remaining gates requires service-owned durable stores, an atomic business-event/outbox path, customer webhook configuration and automated delivery/retry persistence, plus removal of the audited boundary violations. Phase 6 was not started.
