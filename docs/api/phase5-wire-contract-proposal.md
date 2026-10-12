# Phase 5 V1 Wire Contract Proposal

**Status: partially decided.** The product/API definition for
`total_cost_minor` below was provided by the user on 2026-10-11. Other proposed
wire decisions and unresolved implementation gaps remain explicitly identified;
this document does not imply that the original Osai architecture documents
specified the new total-cost definition.

## Confirmed in the available sources

- The Technical Handoff requires quote create/get/accept, trade get/list,
  beneficiary registration, balances, transactions, settlement reports,
  webhook configuration, and entitled provider status.
- Trade-list dimensions are date, currency, status, and customer reference.
- The Technical Handoff describes quote fields as sell/buy currencies and
  amounts, rate, fees, total cost, settlement method/class, estimated
  settlement time, expiry, funding requirement, and acceptance constraints.
- Customer webhook events include `quote.expired`, `trade.accepted`,
  `trade.funding_required`, `trade.funded`, `trade.fx_executed`,
  `trade.settlement_processing`, `trade.settled`, `trade.failed`,
  `trade.reversed`, and customer-relevant `reconciliation.break_created`.
- Customer webhook delivery is at-least-once, uses stable event IDs, exponential
  retry, and supports replay through an API or portal using the same event IDs.
- The Security Design defines
  `HMAC-SHA256(endpoint_secret, event_id || "." || timestamp || "." || body)`,
  an `X-Osai-Event-Id` header, and five-minute timestamp tolerance.
- Customer credentials are environment-scoped; secrets are hashed and shown
  once; clients can self-serve rotation/revocation; rate limits apply per
  client and institution.
- The current OpenAPI already uses `page` (default `1`), `page_size` (default
  `25`), and paginated `{items, page, page_size, total}` responses for
  transactions. Current quote/trade/settlement schemas and service models are
  narrower than the Technical Handoff's full quote description.

## Proposed wire decisions (inference; approval required)

### Trade list and query parameters

Proposed request:

```http
GET /v1/trades?page=1&page_size=25&from=2026-10-01T00:00:00Z&to=2026-11-01T00:00:00Z&currency=NGN&status=SETTLED&customer_reference=partner-order-42
```

| Query field | Type | Required | Proposed meaning |
| --- | --- | --- | --- |
| `page` | integer | No | 1-based page; default `1`, matching `/v1/transactions`. |
| `page_size` | integer | No | Page length; default `25`, matching `/v1/transactions`. A public maximum is not specified in the sources and needs approval. |
| `from` | RFC3339 date-time string | No | Inclusive lower bound on trade creation time. |
| `to` | RFC3339 date-time string | No | Exclusive upper bound on trade creation time. |
| `currency` | string | No | Exact currency filter; proposal applies it to the trade's sell/base currency. |
| `status` | string | No | Exact canonical trade status; do not translate to provider state. |
| `customer_reference` | string | No | Exact institution-scoped caller reference. |

Proposed `200` response:

```json
{
  "items": [
    {
      "trade_id": "trd_...",
      "quote_id": "quo_...",
      "institution_id": "inst_...",
      "status": "SETTLED",
      "base_amount_minor": 10000,
      "base_currency": "NGN",
      "quote_currency": "USD",
      "settlement_id": "si_...",
      "request_id": "req_..."
    }
  ],
  "page": 1,
  "page_size": 25,
  "total": 1,
  "request_id": "req_..."
}
```

`items[]` reuses the existing OpenAPI `TradeResponse`; all fields retain its
existing types and requiredness. The outer page shape follows the existing
transaction response. `from`/`to` names and half-open bounds, base-currency
interpretation, exact matching, and response reuse are inferred choices.

**Customer-reference gap:** `tradecore.Trade` and the current create-quote
request do not contain a customer reference. The filter cannot be truthfully
implemented unless the reference is accepted at quote creation and persisted
through quote acceptance into the owner-local trade record. Proposed addition:
optional `customer_reference: string` to `CreateQuoteRequest`, copied to the
trade and returned on `TradeResponse`. This is an inference about where the
reference originates; alternative is to drop that filter, which conflicts with
the Technical Handoff's stated dimension.

### Settlement report

Proposed request reuses `page`, `page_size`, `from`, `to`, `currency`, and
`status`; dates filter the authoritative settlement transition time, and
`currency` filters settlement currency. The date boundary semantics match the
trade proposal. These filters and meanings are inferred; only the report/export
purpose is explicit.

Proposed `200` response:

```json
{
  "items": [
    {
      "settlement_id": "si_...",
      "trade_id": "trd_...",
      "quote_id": "quo_...",
      "status": "CONFIRMED",
      "amount_minor": 9500,
      "currency": "USD",
      "occurred_at": "2026-10-11T12:00:00Z"
    }
  ],
  "page": 1,
  "page_size": 25,
  "total": 1,
  "request_id": "req_..."
}
```

Proposed item types: IDs/status/currency are strings; `amount_minor` is an
integer; `occurred_at` is an RFC3339 date-time. The listed business fields are
required; `request_id` is required on the response envelope. Do not include
beneficiary account details, provider references, or raw evidence in a
customer report. The row is settlement-owner data joined to trade/reporting
facts only through service APIs, never cross-service SQL.

The current settlement model has transition timestamps but its public gRPC
response has no event timestamp; the service needs to return the authoritative
terminal transition time for this report. Whether the report includes only
terminal settlements or every instruction, and whether export means
downloadable file vs paginated JSON, are unresolved source-level ambiguities.
Recommendation: JSON page containing terminal outcomes for V1; confirm before
implementation.

### Quote response fields

Proposed to preserve current fields and add the missing documented concepts:

| Field | Type | Required | Mapping / proposal |
| --- | --- | --- | --- |
| `sell_amount_minor` | integer | Yes | Alias of current `base_amount_minor`. |
| `sell_currency` | string | Yes | Alias of current `base_currency`. |
| `buy_amount_minor` | integer | Yes | Alias of current `amount_out_minor`. |
| `buy_currency` | string | Yes | Alias of current `quote_currency`. |
| `rate_minor` | integer | Yes | Keep existing field and its existing service meaning. |
| `fee_minor` | integer | Yes | Preserve current single fee amount; do not invent a fee breakdown. |
| `total_cost_minor` | integer | Yes, once the funding-flow inconsistency below is resolved | `base_amount_minor + fee_minor`, denominated in `base_currency` (the source/funding currency). Both operands and the result are integer minor units. |
| `settlement_method` | string | Yes | Customer-facing normalized method/class, not an internal liquidity-provider ID. Exact vocabulary needs approval. |
| `estimated_settlement_time_seconds` | integer or `null` | Yes | Proposed nullable when no reliable estimate exists; the service currently has no estimate field. |
| `funding_requirement` | object | Yes | Proposed `{ "required": boolean, "amount_minor": integer, "currency": string }`; whether `amount_minor` is zero or omitted when not required needs approval. |
| `acceptance_constraints` | array of strings | Yes | Empty array when none; exact stable codes/text are not defined. |
| `expires_at` | RFC3339 date-time string | Yes | Keep current field. |

Retain the existing `base_*`, `quote_currency`, `amount_out_minor`, and
`destination_rail` fields during a compatibility period; new aliases are
additive. Do not expose `ProviderQuote.ProviderID`, raw evidence, or route
candidates. The Handoff explicitly warns against exposing sensitive provider
selection. `destination_rail` is already public today, so whether it is a
customer-approved method or a sensitive provider selector must be confirmed;
removing or changing it would be breaking and is not proposed here.

**Options:** (A) add aliases while preserving current fields (recommended,
additive); (B) rename current fields to sell/buy terminology, which is a
breaking V1 change. The definition of `total_cost_minor` is a product/API
decision supplied on 2026-10-11, not a requirement already stated in the
Technical Handoff. The exact rate scale remains undefined by the available
sources.

#### Product/API decision: total customer cost

The product/API definition supplied on 2026-10-11 is:

- `base_amount_minor` is the exchange principal in `base_currency`, which is
  the source/funding currency.
- `fee_minor` is the total customer-facing fee in that same source currency.
- `total_cost_minor = base_amount_minor + fee_minor`, in integer minor units
  and denominated in `base_currency`.
- `amount_out_minor` remains a separate destination-currency amount in
  `quote_currency`.

For example, a principal of NGN 10,000 minor units with an NGN 100 fee has a
total cost of NGN 10,100 minor units; the destination amount is separate.

#### Existing implementation consistency check

The current code does **not** implement that funding meaning end-to-end:

- `QuoteRequest.BaseAmountMinor` is stored as quote principal, while the
  simulator currently returns `AmountOutMinor` equal to that input regardless
  of currency conversion and returns a fixed `FeeMinor` of 500. Its `RateMinor`
  is not used to calculate the output.
- The quote-to-trade request carries both the base principal and the
  destination `amount_out_minor`, but Trade Orchestrator reserves the latter
  amount in the currency supplied with it (`quote_currency`).
- The fee is not passed into the trade or ledger funding-reservation command.
  The ledger reservation stores one amount/currency per trade and does not
  reserve `base_amount_minor + fee_minor` in the source currency.

Therefore, exposing the defined `total_cost_minor` now would promise a
source-currency customer debit that the existing quote/trade/ledger flow does
not reserve or settle. Do not add the response field or silently change
funding/settlement amounts until the owner-local quote, trade, settlement and
ledger models support separate source funding (including the fee) and
destination payout without violating ledger accounting invariants. The
cross-currency rate calculation and accounting treatment need an owner-approved
design; the public total-cost formula itself is defined above.

#### Two-currency funding and posting model (direction approved; details pending)

This is a design proposal, not an existing architecture requirement or
implementation. The user approved this direction on 2026-10-11: reserve
`base_amount_minor + fee_minor` in `base_currency`, settle
`amount_out_minor` separately in `quote_currency`, and link independently
balanced Ledger journals at verified finality. This approval does not approve
account codes, posting classifications, or full execution-evidence rules. The
user also selected Osai-orchestrated, separate conversion and beneficiary-payout
instructions rather than treating one provider transfer as both legs. FX
execution must support multiple models: initially an approved external
liquidity provider executes conversion; later, Osai treasury may execute when
liquidity capability, infrastructure, risk controls, and regulatory
permissions are in place. Every trade records the actual executor and applies
the evidence requirements for that executor. Ledger finality depends on
verified execution and settlement evidence, not executor identity. The
proposal follows the user-defined source-currency total and current owner
boundaries:

1. **Immutable accepted economics.** Quote/Trade must persist the accepted
   tuple: source principal (`base_amount_minor`, `base_currency`), customer
   fee (`fee_minor`, `base_currency`), total source funding
   (`total_cost_minor`, `base_currency`), destination payout
   (`amount_out_minor`, `quote_currency`), rate, quote ID, and provider/route
   references. Retries must use this accepted tuple, not recalculate it.
2. **Source funding hold.** Trade requests a Ledger-owned reservation for
   `total_cost_minor` in `base_currency`. Ledger continues to own available
   balance, reservation uniqueness, hold, release, and finalization. No gateway
   or other service writes Ledger storage.
3. **Two distinct execution instructions.** Osai orchestrates a conversion
   instruction for `base_amount_minor` in `base_currency` against the accepted
   quote, then, after verified FX completion and destination proceeds being
   available, a separate beneficiary payout instruction for
   `amount_out_minor` in `quote_currency`. An immutable `fx_executor_type` and
   executor ID identify whether the approved external provider or Osai
   treasury executed the conversion. External-provider executions require
   verifiable provider confirmations; Osai executions require Osai execution
   records independently reconciled against relevant cash movements and
   settlement evidence. Each FX instruction and payout has distinct stable
   identifiers, idempotency, persisted status, evidence and reconciliation.
   The payout amount is never reused as the source funding reservation or FX
   debit.
4. **Ledger postings per leg.** After FX completion is verified, Ledger
   consumes source principal from the source hold, accounts for the customer
   fee, and records destination proceeds received. On payout confirmation,
   Ledger records the destination-currency payout from custody against the
   corresponding destination obligation. Source and destination postings use
   independently balanced currency journals linked by trade, FX-execution,
   and payout identifiers. Candidate account roles include source FX
   settlement, destination custody/FX clearing, destination payout payable,
   and a fee payable/revenue account. These are conceptual roles only, not
   approved accounts or accounting classifications.
5. **Evidence-driven Ledger finality.** Ledger verifies the required
   executor-specific FX evidence, source debit, destination proceeds and
   settlement evidence before finalizing corresponding postings. The same
   financial checks apply regardless of who executed the FX trade. Evidence
   must be independently reconcilable and bound to the immutable trade
   economics and stable instruction IDs.
6. **Failure and uncertainty.** A confirmed FX failure releases the full
   source hold and creates no destination payout. An UNKNOWN FX result retains
   the hold and is resolved by polling/reconciling the existing FX instruction,
   never by creating another conversion. If FX succeeds but payout is
   unresolved, the FX trade remains settled while payout status is tracked
   independently. A verified terminal payout failure enters a controlled
   recovery workflow: retry the payout or use an alternate provider route only
   when the previous route is proven not to have paid and the funds' current
   location is verified. If recovery is impossible, execute a compensating FX
   conversion or refund only under an approved policy, verify returned funds,
   and record each movement as new linked Ledger entries. Never delete or
   reverse original settled entries, and never refund before verifying where
   the funds are held. UNKNOWN payout outcomes are reconciled before retry or
   route change. Duplicate and reordered events for either leg must be
   idempotent in Ledger.
7. **Cross-service recovery.** Quote, Trade, Settlement, and Ledger persist
   only in their own stores. Owner-local idempotency and durable outbox/inbox
   patterns carry commands and each leg's finality; no distributed transaction
   is added.

The original same-currency path cannot be generalized by merely changing the
reserved amount: its current Ledger finalization requires the reservation,
settlement amount, and currency to match. Current settlement storage also
permits only one instruction per trade, and the provider interface has no
separate FX-execution leg. Both need owner-local extensions; a destination
transfer alone does not prove that source principal was converted or source
and destination positions were accounted for.

The customer-visible trade and webhook model must distinguish completed FX
from beneficiary payout status. The user-defined recovery policy does not
specify exact public status/event names or payloads; do not map a payout failure
to `trade.failed` or `trade.settled` without an approved state/event contract.

**Further approval still needed before implementation:**

- Which approved external liquidity providers are eligible, and what
  provider-specific source-debit/destination-proceeds confirmations are
  authoritative? For Osai treasury execution, what independent cash accounts,
  records, and reconciliation controls prove source debit and destination
  proceeds? The current code has no FX-execution leg; its settlement provider
  creates only a destination-currency transfer.
- Which Ledger account roles and counterparties represent source FX clearing,
  destination FX clearing/custody, and fee recognition? Should the fee be
  recognized only on confirmed payout, and is it retained, payable, or routed
  elsewhere?
- What exact evidence proves both conversion legs and their amounts/currencies
  before source funding is consumed, and how are those instructions
  authenticated and idempotent?
- Which approving role authorizes a compensating FX/refund policy, and what
  measured funds-location evidence is sufficient to begin recovery?

**Agreed boundary:** keep cross-currency acceptance and exposure of
`total_cost_minor` blocked until Finance/Accounting and the FX execution owner
approve those evidence and accounting details. Continue to support only flows
whose funding and payout are already represented consistently; do not treat
equal test amounts or the simulator as proof of FX accounting.

### Provider-status entitlement and response

**Approved decision:** the public endpoint requires `providers:read` and
returns stable customer-facing route IDs, normalized availability, and
supported capabilities. It does not expose provider IDs, internal health
details, or maintenance fields because no authoritative maintenance schedule
exists.

```http
GET /v1/providers/status
```

```json
{
  "items": [
    {
      "route_id": "simulator",
      "status": "AVAILABLE",
      "capabilities": ["quotes", "fx", "payout"]
    }
  ],
  "request_id": "req_..."
}
```

`items` and `request_id` are required. Each item requires `route_id` (string),
`status` (`AVAILABLE | UNAVAILABLE`), and `capabilities` (array of strings
selected from `quotes`, `fx`, `payout`). Current route identifiers are
`simulator` and `ngn-bank-transfer`. The response and entitlement are
implemented through Quote-owner gRPC and API Gateway.

### Credential lifecycle paths

Proposed customer-management surface:

| Method and path | Proposed request | Proposed `200`/`201` response |
| --- | --- | --- |
| `POST /v1/credentials` | `{ "label": string, "environment": "sandbox"|"staging"|"production" }` | `201` `{ credential_id, public_id, environment, status, created_at, secret }`; `secret` is returned once only. |
| `POST /v1/credentials/{credential_id}/rotate` | `{}` plus required `Idempotency-Key` | `200` with the replacement credential fields and one-time `secret`; old credential remains valid during an overlap window. |
| `DELETE /v1/credentials/{credential_id}` | No body | `204`; subsequent authentication with the credential fails. |

IDs/labels/environment/status/secret are strings; `created_at` is an RFC3339
date-time. `secret` is write-only and never logged or returned again. The
environment values follow the Security Design; paths, methods, labels, response
status codes, idempotency on rotation, and overlap mechanics are inferred.
Existing credential records have no environment field. The design says
zero-downtime rotation but does not specify overlap duration or who authorizes
self-service credential administration. Recommendation: rotate by creating a
replacement while retaining the old key until explicit revocation; do not
choose an overlap timeout without owner approval.

**Blocker:** the existing API-key authentication alone cannot establish whether
a compromised client credential may create/revoke other credentials. Confirm
the authorizing role/bootstrap/admin mechanism before implementing these
routes. Credential lifecycle belongs to Customer/KYB; the gateway may only
translate authorized requests to its owner service.

### Webhook replay path

Proposed path: `POST /v1/webhook-deliveries/{event_id}/replay`, requiring
`webhooks:write` and `Idempotency-Key`. Proposed request body is empty; proposed
`202` response is `{ "event_id": string, "status": "QUEUED",
"request_id": string }`. The operation queues another delivery attempt of the
same immutable event and preserves the same event ID; it does not create or
re-publish a new domain fact. These exact path/status/body/entitlement choices
are inferred from “replay via API/portal using the same event IDs.”

Alternative: `POST /v1/webhook-events/{event_id}/replay` names the resource as
an event rather than a delivery. Recommendation: use
`/v1/webhook-deliveries/{event_id}/replay` because the user action repeats
delivery, while retaining the stable event identifier. Authorization scope,
whether only failed/dead-letter events are eligible, and whether replay history
is separately exposed are unresolved.

### Customer webhook event catalog and signature

Proposed `event_type` enum is exactly:

`quote.expired`, `trade.accepted`, `trade.funding_required`, `trade.funded`,
`trade.fx_executed`, `trade.settlement_processing`, `trade.settled`,
`trade.failed`, `trade.reversed`, `reconciliation.break_created`.

The last event is emitted only when the break is customer-relevant. The names
are explicit in the Technical Handoff; per-event payload fields/version
schemas are not. Keep the current envelope keys `event_id`, `event_type`,
`event_version`, `correlation_id`, `payload`, and `occurred_at`; the object
types remain strings for IDs/type/version/correlation, an object for
`payload`, and RFC3339 date-time string for `occurred_at`. Exact per-event
payload schemas are a blocker to treating the catalog as a stable public
contract.

Signature requirement (documented): lowercase hexadecimal HMAC-SHA256 keyed by
the endpoint secret over the exact UTF-8 bytes
`event_id + "." + timestamp + "." + raw_body`. Send `X-Osai-Event-Id`,
`X-Osai-Timestamp` (RFC3339), and `X-Osai-Signature`; reject timestamps more
than five minutes from receiver time. Delivery remains at-least-once; receivers
deduplicate on the stable event ID. Retry/replay uses that same event ID.

The current implementation and tests now follow this signature input and
five-minute tolerance. This proposal does not change those verified behaviors.

## Source-to-decision traceability

| Topic | Source evidence | Decision in this proposal | Explicit vs. inference |
| --- | --- | --- | --- |
| Trade list and filters | Technical Handoff §7: list trades; filter by date, currency, status, customer reference. OpenAPI `Page`/`PageSize` and `TransactionPage`. `tradecore.Trade` has status, currencies, and timestamps but no customer reference. | Reuse `page`, `page_size`, `TradeResponse`; add `from`, `to`, `currency`, `status`, `customer_reference`. | Dimensions explicit; parameter names, range semantics, match behavior and reference storage inferred. |
| Settlement report | Technical Handoff §7 says settlement report/export. OpenAPI `SettlementResponse`; settlement model includes instruction identifiers, amount/currency/status and transition timestamps. | Paginated JSON of settlement facts; time range/status/currency filters; no provider refs or beneficiary details. | Purpose explicit; JSON rows, fields and filters inferred. |
| Quote fields and two-currency funding | Technical Handoff §7 lists sell/buy economics, fees, total cost, method/class, estimate, funding and acceptance constraints. OpenAPI and quote model expose base/quote amounts, rate, fee, expiry and settlement rail. User decisions on 2026-10-11 define total cost as base principal plus fee in base/source currency, separate Osai-orchestrated conversion and payout instructions, controlled post-FX payout recovery, and multiple FX executor models. | Add additive sell/buy aliases and customer-facing fields; preserve existing fields and avoid provider evidence/IDs. User approved a source hold plus separate conversion and destination payout instructions, linked per-currency Ledger journals, and keeping FX settled distinct from payout failure. Each trade records external-provider or Osai-treasury executor; Ledger finality is evidence-driven and executor-neutral. | Quote concepts are explicit in the Technical Handoff; formula, model direction and recovery principles are user decisions. Provider-specific evidence, treasury cash reconciliation controls, posting classifications, compensation approval, public state/event names, and rate scale remain unresolved. |
| Provider status | Technical Handoff §7: optional capability/maintenance status for entitled clients; quote guidance forbids sensitive provider selection exposure absent commercial approval. | Propose `providers:read` and normalized status/capabilities/maintenance response, with provider identity gated on approval. | Requirement explicit; entitlement key, enum and response inferred. |
| Credentials | Security Design §2: per-environment API keys, hashed one-time secrets, client self-serve rotate/revoke, dual-key zero-downtime rotation. Customer service model has create/revoke primitives but no public lifecycle endpoints or environment field. | Propose `/v1/credentials` create, `/{id}/rotate`, `DELETE /{id}` revoke. | Capabilities explicit; public path/method/schema and authorization bootstrap inferred/blocked. |
| Replay | Security Design §6: replay via API/portal using same event IDs. Technical Handoff §7 requires delivery history. | Propose idempotent `POST /v1/webhook-deliveries/{event_id}/replay`. | Replay capability and stable ID explicit; path, body, status and entitlement inferred. |
| Webhook events/signing | Technical Handoff §7 event list; Security Design §6 signature formula, headers, five-minute tolerance, at-least-once/retry/replay. Current `customer_outbound.go` signs event ID + timestamp + body. | Publish exact event names and envelope; signature formula is retained as existing behavior. | Event names/signing explicit; event payload schemas remain undefined. |

## Compatibility and ownership checks

- Existing endpoints and `TradeResponse`, `SettlementResponse`, transaction
  page names, and idempotency conventions are preserved; proposed quote fields
  are additive. Credential/replay routes are new and do not alter existing
  routes.
- Trades remain trade-orchestrator-owned; settlement report facts remain
  settlement/reporting-owner data; balances remain Ledger-derived. The gateway
  must use typed owner-service APIs and may not query service databases.
- Credential state belongs to Customer/KYB. Webhook event/delivery state and
  signing secrets belong to Notification/Webhook. No cross-service database
  writes or distributed transactions are proposed.
- Amounts are integer minor units. Provider callbacks and customer webhooks are
  evidence/notifications, not balance mutations.
- Provider/customer secrets are never returned after creation, placed in URLs,
  or logged. Tenant scoping is applied at each owner boundary.
- The additive proposal is compatible with existing response fields, but
  customer-reference persistence, public credential authorization, and
  provider identity visibility need approval before any OpenAPI/API change.

## Genuine blockers for approval

1. Resolve the quote/trade/settlement/ledger funding-model mismatch before
   publishing `total_cost_minor`; define the FX rate scale and what constitutes
   “funding required” from the appropriate domain owner. The total-cost formula
   itself is defined above by the user.
2. Confirm whether `customer_reference` is a new client-supplied request field
   and its lifecycle/uniqueness rules.
3. Approve settlement-report contents (terminal-only vs. all instructions),
   timestamp meaning, JSON vs. file export, and public pagination limits.
4. Name the provider-status entitlement and approve provider identity exposure
   vs. customer-facing route IDs; define public maintenance fields.
5. Confirm credential administration authority/bootstrap, rotation overlap
   semantics, and whether list/delete route naming and idempotency align with
   the partner credential-management model.
6. Confirm webhook replay eligibility and whether delivery history needs a
   read endpoint; define each event's versioned payload schema.

The total-cost product definition is recorded above, but the response field is
not implemented because the existing source-currency funding and destination
settlement behavior is inconsistent with that definition. Other unresolved
wire decisions in this document have not been implemented.
