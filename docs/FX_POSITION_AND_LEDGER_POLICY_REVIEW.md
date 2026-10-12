# FX Position and Ledger Policy Review

**Status: engineering proposal for Finance, Treasury, Legal/Compliance, and
operations review. It is not formal accounting approval or authority to post
FX entries.**

The engineering baseline is V1 NGN/USD with Osai controlling commercial
pricing and treasury exposure. The product assumes USD capacity is pre-funded,
but this repository does not establish the legal owner, custody location, or
contractual USD rights. The specific chart-of-accounts mappings, debit/credit
directions, valuation and fee-recognition policies, custody classification,
and reversal treatment are not approved. Production cross-currency execution
and posting must remain disabled until those facts, approvals, and the durable
Ledger implementation are complete.

This proposal assigns recommended accounting **roles**, conditional journal
shapes, and evidence/state requirements. The proposed account keys and journal
examples are not approved chart mappings or instructions to post. They must be
adapted to the actual legal entity, provider/custody contracts, and Finance
decisions below. The older examples in
[the historical ledger money-model document](../osai_ledger_money_model_v0.2.md)
are illustrative only and are not approval.

## USD custody evidence checked in this repository

The repository does not establish where Osai's USD is held or who owns or
controls it. The Flutterwave adapter is configured for transfer access through
URL/credential settings, validates outbound transfers as NGN, and exposes no
USD balance or USD custody-account evidence. The provider adapter contract is a
pre-code proposal, not an executed provider agreement. The local provider
handoff describes a test-only NGN prefund fixture and safeguarded NGN custody
posting; it explicitly says that fixture is not evidence of real bank funding.
No executed USD provider, bank, or custody agreement, USD account statement,
or verified USD custody configuration is present in the repository.

Accordingly, no USD asset model can be recommended as the existing V1
arrangement. The exact evidence needed to decide is the executed agreement for
the selected NGN/USD route, including the named legal account holder and
beneficial owner of prefunded USD, the custodian/provider and account location,
Osai's rights to withdraw or direct payout, and whether USD is owed trade by
trade or owned inventory. An independently obtained account statement or
custodian confirmation must corroborate the actual account and control.

The mapping follows that evidence: Osai-controlled and Osai-owned funds map
to the applicable USD custody/cash or treasury-inventory asset; a contractual
right to receive USD from a provider maps to a provider claim/receivable; funds
beneficially owned by customers remain safeguarded customer funds and must not
be represented as Osai inventory. Pooled inventory cost-basis accounting is
required only if evidence establishes Osai-owned fungible USD inventory that
is drawn down to execute trades. A trade-specific provider claim alone does
not establish such an inventory pool. Until the agreement and independent
custody evidence are supplied and reconciled, USD asset mapping, inventory
cost-basis postings, and cross-currency production posting remain disabled.

### NGN/USD route evidence matrix

This matrix records evidence checked in the repository, not a claim that
external contracts or statements do not exist outside it.

| Requirement | Evidence found | Verification status | Exact remaining gap | Production consequence |
| --- | --- | --- | --- | --- |
| Provider and executable route | [Flutterwave outbound adapter](../adapters/flutterwave/outbound.go) validates transfers as NGN and exposes create/query/statement paths. Its configuration is URL, credentials, environment, and enable switch; repository files do not identify a USD FX executor or a real configured account. The [adapter contract draft](../osai_provider_adapter_contract(1).md) is explicitly pre-code. | **Verified repository facts; no executed route agreement found.** | Agreement for the selected NGN/USD route naming the FX executor, payout executor, contractual transfer of each currency, fees, settlement timing, and query/statement evidence available. | Do not enable cross-currency real-money execution or claim provider settlement. |
| USD account ownership and custody | Flutterwave outbound only accepts NGN. No USD custody account, account statement, or custody configuration is present in the checked repository. | **Not established.** | Executed account/custody agreement and independent provider/bank statement or custodian confirmation establishing legal holder, beneficial owner, location, control/withdrawal rights, and trade-by-trade claim versus Osai-owned inventory. | No USD asset mapping or USD posting. Cost-basis accounting is required only if Osai-owned fungible inventory is evidenced. |
| NGN source prefunding | The [local provider handoff](../docs/handoffs/provider-sandbox-flutterwave-handoff.md) describes a proof-only prefund fixture posting to NGN custody/prefund roles and says it is not evidence of real bank funding. | **Test mechanism verified; real funding not verified.** | Production source-account/custody contract and matched independent NGN bank/custodian movement for the route. | Do not treat the local fixture as production customer funds or use it to enable real transfers. |
| Customer quote and failure terms | Quote code implements fixed-source and fixed-destination economics, fee schedule, currency precision, integer rates, and rounding. The wire-contract draft is not customer contract evidence. | **Engineering behavior verified; route-specific terms not verified.** | Confirm the terms actually presented/accepted for fee, destination amount/surplus, expiry, and post-FX payout failure/refund. Engineering defaults below apply without further policy approval but must match the live customer terms before launch. | Do not expose an ambiguous total-cost field or launch terms inconsistent with implemented behavior. |
| Ledger account mappings | Ledger FX helpers accept explicit, currency-matched account details; repository has test placeholders, not production account IDs or a verified chart mapping for the route. | **Validation primitive verified; production mapping absent.** | Actual route/entity account IDs and currency/owner dimensions derived from the executed funding/custody contracts and chart of accounts. | FX journal construction/posting stays disabled; never substitute sample or illustrative codes. |
| Execution and settlement evidence | Provider abstractions and Flutterwave transfer query/statement adapters exist; current full flow protects payout finality with provider query evidence. No real FX execution evidence, USD custody movement, or production reconciliation sample is in the repository. | **Interfaces/test flows exist; FX/custody evidence unverified.** | A route-specific evidenced FX execution sample and independently matched source/destination settlement records, including exact amounts, rate, fees, references, and timestamps. | Keep FX finality/reconciliation unresolved; no automatic retries or balance changes on API success/callback alone. |
| Simulation boundary | Simulator is a separate adapter (`sim_lp_1`); Flutterwave requires explicit sandbox/test configuration and a transfer enable switch. Local proof funding is gated and test-only. | **Separation verified in code/docs; real settlement not implied.** | No additional external evidence to keep simulation separate; confirm production deployment does not set simulator/proof toggles. | Simulator may test state machines only; its quotes, balances, and statuses are never evidence of provider funding or settlement. |

## Current engineering status

Fee-bearing quote acceptance now carries the hashed pricing snapshot from
Quote through Trade into the Ledger funding command. Ledger validates the
snapshot against its hash, source principal, fee, total debit, and currency,
then persists the complete snapshot atomically with the all-in customer hold.
PostgreSQL-backed Trade and Ledger tests cover persistence, restart reads,
duplicate/concurrent replay, and conflicting economics.

This is not fee recognition or FX finality. The funded fee-bearing Trade
remains `FEE_ACCOUNTING_REQUIRED`; no payout is created until the fee has a
safe Ledger disposition. Cross-currency Trade acceptance remains fail-closed
because durable FX execution and corresponding Ledger posting/recovery are
not yet implemented. `ledgercore` FX evidence validation and journal-building
helpers remain primitives, not a durable execution/posting lifecycle. The
expanded independent-services fee-flow test has not passed: its latest build
stopped before service startup because the local C: volume ran out of space.
See the authoritative engineering and test checklist in
[V1_PILOT_READINESS.md](./V1_PILOT_READINESS.md).

## Engineering constraints

- Keep amounts as integer minor units with an explicit currency. Never use
  floating-point arithmetic for amounts, rates, fees, cost basis, or journal
  totals.
- The Ledger remains the only financial source of truth. Balances are derived
  from persisted journals; holds affect spendability, not total customer
  liabilities.
- Keep NGN and USD journal legs separate. Each posted journal must balance in
  its own currency; do not net currencies or use a balancing plug.
- Preserve the accepted quote's economics as an immutable snapshot, separately
  from actual execution economics.
- Separate FX principal, customer fees, provider fees, payout movements, and
  any treasury gain or loss. Do not recognize a fee or trading result from
  quote acceptance, a successful API response, or a callback alone.
- Provider callbacks are evidence or wake-up hints. Finality requires the
  configured provider query/reconciliation evidence and a guarded, idempotent
  Ledger operation.
- Unknown outcomes remain unresolved. Do not retry a transfer create blindly,
  release a source hold, or post a compensating entry when execution or payout
  finality is uncertain.
- Preserve the original accepted quote and journal history. Corrections must
  be linked, separately authorized, and supported by evidence.

## Recommended V1 accounting decisions

These defaults follow the approved product constraint that Osai controls
commercial pricing and treasury exposure, and the existing system design that
uses pre-funded USD capacity. They are recommendations for the Osai decision
maker, not claims of external approval. Where the facts depend on a contract
with a bank/provider or on the legal entity actually holding funds, the
recommendation is conditional on that evidence.

### 1. Principal execution with pre-funded USD inventory

**Recommendation:** Model the customer quote as an Osai principal transaction:
the customer owes the immutable NGN total debit and Osai owes the immutable USD
destination amount. Track source and destination currency legs independently;
never net NGN and USD balances. Use the actual settlement contract to decide
whether the source side is custody cash, a provider receivable, or a provider
payable, and whether USD capacity is custody cash, a provider claim, or inventory.

For the pre-funded route, maintain a trade-specific clearing/control account
for the source obligation and a separate USD inventory/control position. The
accepted quote links the customer's NGN hold to the contracted USD obligation.
At verified FX execution, move the trade from quote economics to actual
execution economics through those control accounts; clear each control only
when its respective source settlement and USD custody/payout evidence arrives.
The control-account mapping must make each currency journal independently
balance and must not plug a residual into customer liability or revenue.

**Illustrative role-only posting shape; not yet postable:**

| Event | NGN entries | USD entries |
| --- | --- | --- |
| Customer prefund evidenced | Debit `CUSTODY_NGN`; credit `CUSTOMER_AVAILABLE_NGN` | — |
| Quote accepted / full debit reserved | Debit `CUSTOMER_AVAILABLE_NGN`; credit `CUSTOMER_RESERVED_NGN` | No USD customer asset or liability is created by quote acceptance. The accepted destination amount is an immutable contractual memorandum/control value until execution is verified. |
| FX execution verified, before cash/custody movement | Debit `CUSTOMER_RESERVED_NGN`; credit `SOURCE_FX_CLEARING_NGN` for the source principal; reclass the agreed customer fee to `FEE_DEFERRED_NGN` (both sides and timing await the fee decision below) | Debit `USD_FX_CLEARING_OR_PROVIDER_CLAIM`; credit `CUSTOMER_PAYOUT_OBLIGATION_USD` for verified proceeds. Use `TREASURY_INVENTORY_USD` instead of a provider claim only if the execution/custody evidence proves that the transaction consumes Osai-owned USD inventory and Finance accepts the linked valuation bridge. |
| Source NGN settles | Debit `SOURCE_FX_CLEARING_NGN`; credit the evidenced NGN custody/cash position, or the reverse clearing direction if the executed contract makes the provider owe Osai | — |
| USD custody confirmed | — | Debit `USD_CUSTODY_OR_INVENTORY`; credit `USD_FX_CLEARING_OR_PROVIDER_CLAIM` |
| Beneficiary payout confirmed | — | Debit `CUSTOMER_PAYOUT_OBLIGATION_USD`; credit `USD_CUSTODY_OR_INVENTORY` (or an approved payout-clearing role if the custodian/provider statement shows an intermediate balance) |

Each row represents separate owner-local postings and is illustrative of the
role linkage, not a complete journal prescription. In particular, actual
pre-funded-inventory consumption needs an approved functional-currency
valuation/release method; it must not be silently represented as a new provider
receivable. Do not recognize a trading gain/loss merely by forcing these
currency-native legs to balance.

**Rationale:** this separates customer obligations from where funds are held,
prevents API/provider status from being treated as custody evidence, and
supports reconciliation by currency and counterparty. If the provider contract
instead has Osai deliver NGN to the provider and receive a USD claim, use
provider payable/receivable roles until independently evidenced settlement;
do not label either side cash before custody is proven.

**Failure treatment:** before a verified execution, keep the entire NGN
reservation. After execution, keep both currency controls and the customer USD
obligation open until each side is evidenced. A missing source wire, missing
USD custody credit, or partial settlement becomes an open reconciliation break,
not a customer balance adjustment.

**Implications:** principal treatment may make Osai bear rate, liquidity,
counterparty, and inventory risk. Contractual control of pricing alone does not
determine legal/accounting principal status; review the customer and provider
contracts and applicable safeguarding/custody rules.

### 2. Customer fee recognition

**Recommendation:** Compute the fee at quote time under the approved,
versioned quote schedule, show it separately, and include it in fixed-source
all-in debit. Keep it deferred/non-revenue while execution or payout is
incomplete. Recognize it only when the customer-facing service obligation
defined in the contract is satisfied; the conservative V1 default is verified
successful beneficiary payout. Do not recognize fee revenue at quote acceptance,
provider acceptance, FX execution, or callback receipt.

**Illustrative role-only entries:** at acceptance, the fee remains part of the
NGN customer reservation; at verified FX execution, move it to
`FEE_DEFERRED_NGN` only if the NGN source obligation has actually been consumed;
at final successful payout, debit `FEE_DEFERRED_NGN` and credit
`CUSTOMER_FEE_REVENUE_NGN`. For a pre-payout cancellation or authoritative
failure, release/refund the deferred fee to customer availability under the
source-currency hold/refund flow. Never credit revenue and customer liability
in the same posted entry to manufacture balance.

**Rationale:** acceptance is not performance and execution can be reversed,
unsettled, or followed by failed payout. This avoids revenue before the
customer receives the contracted service.

**Failure treatment:** if payout is unknown, fee remains deferred. If payout
fails before FX, return the entire source debit. If FX executed but payout
fails, retain fee deferral while the recovery choice is unresolved. A later
customer refund uses a new linked event, never mutation of the accepted quote.

**Implications:** this is a conservative service-completion policy. Finance
must confirm the applicable revenue-recognition standard and whether the
contract's performance obligation is FX execution, making USD available, or
beneficiary receipt. Provider fees are not netted against customer fee revenue.

### 3. Treasury cost basis, FX gain/loss, and rounding

**Recommendation:** For fungible Osai-owned pre-funded USD within one legal
entity and custodian pool, use a moving weighted-average historical cost basis
in the approved functional currency. Keep cost as an exact rational value of
integer minor units and round only at the individual realized disposal posting.
No mark-to-market entries in V1. Separate provider fees, customer fees, and FX
trading result. Use the accepted quote for the customer obligation; record
actual provider execution independently.

The inventory release requires a paired reduction in the USD inventory
quantity and its carrying cost in the functional currency. Record realized
gain/loss as the difference between source consideration attributable to FX
principal (excluding customer fee and provider fee) and the released carrying
cost. That functional-currency journal must balance through explicit
`FX_REALIZED_GAIN` / `FX_REALIZED_LOSS` accounts and the approved
`FX_INVENTORY_COST_BASIS` clearing role; never create a USD-valued plug in the
NGN journal. No entry is permitted if the valuation bridge cannot be
reconciled to the source and destination evidence.

For rounding, preserve the quote engine's existing rules: fee percentage rounds
up to a whole source minor unit; positive currency conversion rounds down;
fixed-destination principal is the smallest source principal whose rounded
proceeds meet the requested target; calculated proceeds/surplus remain explicit.
Do not allocate the fixed-destination surplus to revenue or the customer until
the customer contract says who owns it.

**Rationale:** weighted average is operationally deterministic for fungible,
pooled inventory; exact rational arithmetic prevents drift; explicit surplus
prevents silent underfunding or overstatement.

**Failure treatment:** if cost layers or available inventory cannot be
reconstructed, stop new inventory-consuming trades and open a valuation
reconciliation break. If actual execution rate/proceeds disagree with the quote,
do not rewrite the quote: route to an exception state and apply only the
contractually approved slippage treatment. Unknown trades are not realized.

**Implications:** the applicable treatment may be governed by IFRS 9, IAS 21,
IFRS 15, or local statutory rules depending on contract substance, instrument
classification, and jurisdiction. These are review flags, not a conclusion
that a specific standard applies.

### 4. Failed payout, refund, returned funds, and compensation

**Recommendation:** Treat payout submission, payout finality, and returned
funds as distinct state transitions. Never automatically unwind a completed
FX execution or promise an NGN make-whole after USD payout failure. First
confirm whether the payout was never sent, rejected before debit, debited then
returned, or remains unknown. Preserve the customer's USD obligation until
either verified payout or an explicitly authorized resolution.

| Scenario | Recommended customer/ledger treatment | Required evidence |
| --- | --- | --- |
| Payout rejected before provider debit | Keep USD obligation open until the provider confirms rejection and funds remain in Osai custody; then retry only with the same stable idempotency reference if the provider contract proves the prior instruction cannot later execute, otherwise use manual review. | Query by client/provider reference; exact currency/amount/beneficiary; provider rejection; custody balance/movement. |
| Payout unknown or timeout | No retry, refund, hold release, or compensation. Keep the settlement `UNKNOWN`/manual review and reconcile by reference. | Authoritative provider query plus statement/transaction evidence resolving whether debit or payout occurred. |
| Payout failed and USD is confirmed returned | Record a new, linked USD return movement into the proven custody/provider position; keep source FX and fee accounting intact pending the user's recovery choice. | Return transaction ID, original payout reference, amount/currency, custody credit. |
| Customer requests source-currency refund after FX | Obtain a new FX unwind/reconversion at the then-current executable rate; record actual source and destination legs separately, current value, fees, and any realized gain/loss. Any shortfall is not automatically Osai-funded. | Customer authorization, provider execution evidence, source debit/credit, USD return and custody statement, approved fee/valuation policy. |
| Osai chooses to make customer whole | Require authorized maker/checker approval and a separately funded compensation journal using Osai house funds; do not debit other customers or suspense. | Approved compensation reason/amount, source-of-funds evidence, maker/checker, customer notice, linked original trade and payout break. |
| Duplicate/late success after failure | Keep original entries immutable; quarantine in manual review and post only one new idempotent recovery/compensation event after matching both provider and custody evidence. | Unique provider event and transfer references; independently matched statement movement. |

**Rationale:** FX is irreversible at the original rate and a payout return does
not recreate the original NGN economics. Automatic reversal could over-credit
the customer or create an unfunded liability.

**Implications:** disclose who bears post-execution rate movement and payout
fees in the customer contract. Safeguarding, money-transmission, complaints,
and unclaimed-funds rules may constrain holding or returning funds. The
compensation recommendation is a product risk decision and requires your
explicit approval.

### 5. Provider payable/receivable and payout-clearing mapping

**Recommendation:** classify balances from contractual rights and independent
custody evidence, not provider naming or API state:

- use a provider receivable only when Osai has an enforceable right to receive a
  specified amount from that provider;
- use a provider payable only when Osai owes the provider a specified amount;
- use cash/custody only after the bank/custodian account movement is evidenced
  and the legal entity/beneficial ownership is known;
- use payout clearing only for an actual in-flight settlement balance supported
  by provider/bank statements; clear it only when matching evidence exists;
- keep pending instructions and unknown results in operational state/outbox
  records, not as balances or final journals.

For each configured route, require separate source-currency and
destination-currency mapping sets keyed by provider, legal entity, currency,
and custody arrangement. Production startup must fail closed when a mapping
is absent, roles collapse to the same account, currencies mismatch, or the
mapping cannot produce independently balanced journals.

**Decision required:** approve which contractual counterparty owns source
clearing and whether USD capacity is held as Osai cash, safeguarded client
funds, a provider receivable, or a restricted treasury asset. This determines
whether these entries are balance-sheet assets/liabilities and whether client
funds may be used at all.

### 6. Evidence required for each posting and finalisation

1. **Prefund:** bank/custodian statement ID, unique line reference, account and
   legal entity, exact currency/minor amount, value date, institution
   allocation basis, and independently matched ledger receipt. A transfer
   initiation or webhook is not funding evidence.
2. **Hold:** immutable accepted quote ID/hash and version, customer/institution,
   expiry, exact full source debit, fee schedule/version, beneficiary/route,
   idempotency key, and successful owner-local reservation transaction.
3. **FX execution:** named executor/contract, stable trade and execution refs,
   authoritative query or signed settlement confirmation, exact source debit,
   destination proceeds, applied rate plus direction/precision, explicit
   per-currency fees, timestamps, and raw-payload hash. Recalculate proceeds
   using exact arithmetic and the quote's minor-unit exponents/rounding before
   accepting.
4. **Source settlement:** statement line or provider transaction proving exact
   NGN movement, counterparty, currency, amount, value date, and reference.
5. **USD custody:** custodian/provider account movement or statement proving
   exact USD credit and who owns/controls the claim. A provider execution
   response alone is not proof of custody.
6. **Payout finality:** stable client and provider refs; verified provider
   status query; exact amount/currency/beneficiary; completed time; and
   independent account/statement evidence for actual debit. Callback is a
   hint only.
7. **Return/refund/compensation:** original event linkage, independently
   verified reverse movement or customer instruction, authorized resolution,
   valuation at actual new execution rate, and maker/checker for Osai-funded
   compensation.

Persist raw evidence hashes and normalized fields durably before advancing
final state or posting. Database failure, missing references, amount/rate
mismatch, duplicate reference with different payload, or uncertain finality
must leave the operation unresolved and must not trigger blind retries.

## Required accounting roles

These are semantic roles, not chart-of-accounts names or classifications.
The proposed identifiers below are semantic mapping keys, not existing chart
codes. Configure the final account IDs and legal classifications only after
the Osai decisions below and the relevant custody/provider contracts are
recorded. The current Ledger helper checks that explicit account details and currency
dimensions are present and that each currency leg uses distinct accounts; it
cannot verify external approval, ownership, or a production chart-of-accounts
mapping.

| Proposed mapping key | Currency / owner dimensions | Purpose and boundary |
| --- | --- | --- |
| `CUSTOMER_AVAILABLE` | NGN or USD; institution | Amount Osai owes the customer and which remains spendable. Derived from the Ledger. |
| `CUSTOMER_RESERVED` | NGN or USD; institution and trade | Customer funds reserved for an accepted, funded operation. A hold is not revenue or a reduction in total customer liability. |
| `SOURCE_FX_CLEARING` | NGN; actual counterparty, custodian, or legal entity | Trade-specific source amount due to/from the executor. Account classification follows the executed contract. |
| `USD_PROVIDER_CLAIM` | USD; legal entity and actual provider | Enforceable USD claim against the execution provider before custody credit, only where contract and statements support that claim. |
| `USD_TREASURY_INVENTORY` | USD; Osai legal entity and inventory pool | Osai-owned, pre-funded USD position, only where legal ownership and restrictions permit it. It is not interchangeable with safeguarded customer cash or a provider receivable. |
| `CUSTOMER_PAYOUT_OBLIGATION` | USD; institution and trade | Contracted destination amount owed to the customer/beneficiary, distinct from source principal and fees. |
| `PAYOUT_CLEARING` | Payout currency; provider and settlement | Actual in-flight payout balance supported by provider/bank statements. Not proof the beneficiary received funds. |
| `CUSTOMER_FEE_DEFERRED` | NGN; institution and trade | Customer fee collected/allocated but not yet recognized as revenue. |
| `CUSTOMER_FEE_REVENUE` | NGN; Osai legal entity | Fee revenue after the approved performance-obligation trigger. |
| `PROVIDER_FEE_PAYABLE` | Actual fee currency; provider | Amount owed to provider for a documented fee. |
| `PROVIDER_FEE_EXPENSE` | Actual fee currency; Osai legal entity | Provider cost when the fee is incurred and supported. Do not net against customer fee revenue. |
| `FX_SPREAD_DEFERRED` | NGN; trade and legal entity | Contract-to-execution spread held out of revenue until both FX legs settle and the valuation rule is satisfied. |
| `FX_SPREAD_REVENUE` | NGN; Osai legal entity and trade | Contracted customer-principal margin recognized only after both FX legs settle; distinct from customer fee and inventory gain/loss. |
| `FX_REALIZED_GAIN` / `FX_REALIZED_LOSS` | Approved functional currency; legal entity and inventory pool | Realized inventory cost-basis result only under an approved carrying-value method. |
| `FX_ROUNDING_RESIDUAL` | Currency of residual; trade | Explicit minor-unit residue. Do not post it to revenue or customer funds without the selected beneficiary rule. |
| `RECONCILIATION_SUSPENSE` | Evidence currency; provider/custodian and break | Durable unmatched movement investigation. A break does not create a customer balance or authorize a balancing adjustment. |

### Illustrative balanced journals

This example uses proposed mapping keys, never existing chart codes. Amounts
are minor units. Assume an accepted NGN fixed-source quote with a principal of
150,000, a customer fee of 500, and an all-in debit of 150,500. A verified
provider execution costs 148,000 NGN and produces 10,000 USD. The 2,000 NGN
difference is a candidate contracted principal spread, not automatically
recognized revenue. In this example it is calculated from the accepted
principal and the provider's evidenced NGN execution cost; it is not an
unexplained balancing residual.

| Event | Debit | Credit | Gate |
| --- | --- | --- | --- |
| Prefund evidenced (NGN) | `CUSTODY_NGN` 150,500 | `CUSTOMER_AVAILABLE` 150,500 | Matched bank/custodian receipt. |
| Quote accepted / reserve (NGN) | `CUSTOMER_AVAILABLE` 150,500 | `CUSTOMER_RESERVED` 150,500 | Immutable quote + owner-local hold transaction. |
| FX source leg executed (NGN) | `CUSTOMER_RESERVED` 150,500 | `SOURCE_FX_CLEARING` 148,000; `FX_SPREAD_DEFERRED` 2,000; `CUSTOMER_FEE_DEFERRED` 500 | Accepted principal, separately stated customer fee, and provider's evidenced NGN execution cost support each component; no unexplained plug. |
| FX destination leg executed (USD) | `USD_PROVIDER_CLAIM` 10,000 | `CUSTOMER_PAYOUT_OBLIGATION` 10,000 | Verified destination proceeds and enforceable provider obligation. For inventory draw, replace the claim only after approved mapping and cost-basis rules. |
| Source leg settles (NGN) | `SOURCE_FX_CLEARING` 148,000 | `CUSTODY_NGN` 148,000 | Matched NGN statement movement. |
| USD credited to custody (USD) | `CUSTODY_USD` 10,000 | `USD_PROVIDER_CLAIM` 10,000 | Matched USD custodian/provider statement. |
| Both FX legs settle (NGN) | `FX_SPREAD_DEFERRED` 2,000 | `FX_SPREAD_REVENUE` 2,000 | Both legs settled; valid source cost/quote comparison; accepted treatment of principal spread. |
| Beneficiary payout completes (USD) | `CUSTOMER_PAYOUT_OBLIGATION` 10,000 | `CUSTODY_USD` 10,000 | Provider query plus independently matched beneficiary debit. |
| Fee performance obligation completes (NGN) | `CUSTOMER_FEE_DEFERRED` 500 | `CUSTOMER_FEE_REVENUE` 500 | Contractual service-completion event, recommended default: payout completed. |

The example is internally balanced within each currency at every row. It
assumes an execution provider owes the USD proceeds; it does not describe the
USD asset when funds are already held in an Osai-owned inventory account. For
that case the USD inventory release and NGN carrying-cost/gain bridge must be
approved and tested before posting. If fees are charged in a currency other
than NGN, use a separate journal in that fee currency and its actual payable
mapping.

## Currency-specific journal template catalogue

The following templates describe which facts and roles belong to a posting
boundary. They do not prescribe debits, credits, account IDs, or missing
balancing entries. No template is postable until Finance approves its complete
mapping and the Ledger validates it transactionally.

| Template | Currency leg(s) | Roles in scope | Required evidence / posting boundary |
| --- | --- | --- | --- |
| Customer prefund | One source currency, e.g. NGN | Custody/funding position; customer available obligation | Independently matched bank/custodian movement, exact currency and amount, institution allocation, and unique evidence reference. |
| Quote acceptance / reservation | NGN for the NGN/USD V1 source leg | Customer available obligation; customer reserved obligation | Immutable accepted quote, expiry, fee breakdown and total source debit. Only a hold/reclassification is eligible; no FX completion or fee revenue. |
| FX execution — source leg | NGN only | Customer reserved obligation; approved source clearing/treasury role; fee tracking only if separately supported | Verified executor, exact NGN FX principal (distinct from total customer debit and separately evidenced fees), actual rate and execution reference, and source settlement evidence. The source-currency journal must balance independently. |
| FX execution — destination leg | USD only | Treasury inventory/provider claim; customer destination payout obligation | Verified USD proceeds, actual applied rate applied to the source principal, executor reference, custody/claim basis, and corresponding source trade identity. The USD journal must balance independently. |
| Destination custody confirmation | USD only | Treasury inventory/provider claim; actual custody/provider position | Custodian statement, account transaction, or independently verifiable provider balance and movement. An FX execution response alone is insufficient. |
| Payout submission | Payout currency | Payout rail/provider clearing; customer destination payout obligation | Submission is an operational state, not proof of payment. Do not post customer finality solely because a request was accepted. |
| Payout confirmation | USD in the V1 destination flow | Customer destination payout obligation; actual custody/provider position; payout clearing where approved | Verified final beneficiary movement, stable provider and client references, exact amount/currency, and matched beneficiary. |
| Failure before FX execution | Source currency only | Customer reserved obligation; customer available obligation | Authoritative evidence that execution did not occur. Unknown or ambiguous execution leaves funds reserved and unresolved. |
| Payout failure after FX execution | USD and NGN as separately evidenced | Destination custody/provider claim; payout obligation; source customer/treasury roles only under an approved recovery policy | Locate and verify returned or recoverable USD first. Do not release the original NGN hold or create a make-whole/reversal journal automatically. |
| Returned funds / reversal | Each evidenced currency in its own journal | Actual custody/provider position; linked original trade and settlement roles | A return is a new externally evidenced movement linked to the original instruction. It does not automatically restore a customer balance or reverse original journals. |
| Reconciliation difference | Currency of the unmatched movement | Durable break record and, only after approval, mapped suspense/adjustment roles | Preserve statements, provider evidence, ledger comparison, maker/checker decision, and resolution reference. Opening a break creates no customer balance or automatic adjustment. |

Every eventual template must have an explicit idempotency key and immutable
trade/quote/settlement linkage. Currency-specific balance checks must reject
incomplete or unbalanced posting sets. No system should use a plug entry to
make a journal balance.

## Evidence and state-transition requirements

| Operation state | Minimum evidence to advance | If evidence is missing, mismatched, or uncertain |
| --- | --- | --- |
| Quote offered | Currency pair, amount, rate direction, quote mode, fee rules, expiry, and opaque quote ID | Reject invalid pairs/ranges. Do not expose an undefined total-cost field. |
| Quote accepted | Authorized institution/credential; unexpired immutable quote; canonical request hash | Replay the same accepted response for the same idempotency key. Reject a different payload; do not duplicate the hold. |
| Funds reserved | Owner-local Ledger transaction tying institution, trade, source currency, total debit, and reservation ID | Keep trade funding-required; do not submit payout or FX. |
| FX submitted | Identified executor and idempotent execution request tied to the accepted quote/trade | On timeout or ambiguous response, retain the hold and reconcile/query the same execution reference. No blind resubmission. |
| FX executed | Verified executor-specific evidence for source debit, USD proceeds, applied rate, fees, references, and timestamps | Keep execution unknown/manual review. Do not recognize treasury gain/loss, fees, or payout completion. |
| USD custody confirmed | Bank/custodian statement or independently verifiable provider account movement | Keep destination funds unresolved; do not describe them as safeguarded cash or available for payout. |
| Payout submitted | Approved beneficiary, exact amount/currency, provider reference, and stable settlement ID | Remain processing/unknown; do not consume the customer obligation or retry a create blindly. |
| Payout confirmed | Authoritative provider query and independent finality evidence matching amount, currency, beneficiary, and references | Mismatch goes to manual review. A callback alone is never enough to post finality. |
| Payout failed/returned | Authoritative failure/return evidence and confirmation of whether FX already executed and where destination funds reside | Keep the trade unresolved until location and recovery are evidenced. No automatic source release or compensation. |
| Reconciliation resolved | Matched bank/provider statement, Ledger comparison, approved disposition, and maker/checker evidence | Keep the break open; never auto-post a difference to the customer or P&L. |

An API success, signed callback, accepted provider transfer, or Temporal signal
is not by itself financial finality. Reordered and duplicate events must remain
idempotent and must not create duplicate money movements.

## Treasury inventory and realized FX result proposal

**Proposed for review only; not an approved valuation or posting policy.**

For a fungible, pre-funded USD pool owned by one legal entity and held at one
identified custodian, Finance/Treasury could evaluate a weighted-average
historical cost basis. Keep the basis as an exact rational amount derived from
integer NGN and USD minor units, partitioned at minimum by legal entity,
custodian/provider, currency, and any policy-required inventory pool. Do not
use floating point. Do not pool positions whose ownership, restrictions, or
economic rights differ.

At an FX disposition, a candidate gross realized result in NGN would compare
the approved NGN consideration attributable to the USD principal with the
carrying cost allocated to the USD minor units delivered. Customer fees and
provider fees remain separate. This is a measurement proposal only: Finance
must decide the functional currency, eligible cost components, pool boundaries,
allocation method, rounding point/mode, treatment of partial units, and
recognition date before any value is calculated or posted.

In notation, the candidate allocation is:

```text
weighted_cost_per_USD_minor =
    eligible_NGN_carrying_cost_minor / eligible_USD_inventory_minor

allocated_cost_NGN_minor =
    delivered_USD_minor * weighted_cost_per_USD_minor

candidate_realized_result_NGN_minor =
    approved_NGN_consideration_for_principal - allocated_cost_NGN_minor
```

The ratio and allocation remain exact rational values until a Finance-approved
posting boundary determines the rounding rule. This equation is a review
proposal, not an instruction to calculate or post a gain/loss today.

Open-position valuation, impairment, unexecuted quotes, quote-to-execution
slippage, rounding residuals, provider charges, failed payouts, returned funds,
and reversals need explicit policy. An unknown execution must not be treated as
a realized result. Original entries are not overwritten; approved corrections
must be linked compensating events.

## Required scenario tests before enabling postings

- Fixed-source and fixed-destination quotes: exact all-in debit, principal,
  destination proceeds, fee rounding, expiry, and immutable quote replay.
- Per-currency journal balancing: source and destination legs independently
  balanced, no cross-currency netting, no float arithmetic, overflow rejection.
- Duplicate and concurrent acceptance/execution/payout requests: one hold and
  one economic effect; conflicting idempotency payloads rejected.
- Callback-first, duplicate, reordered, mismatched, and stale events: evidence
  recorded, provider queried, no callback-only financial finality.
- Unknown execution, provider timeout, process termination, worker/service
  restart, and database outage: no blind create retry, no premature release,
  and safe recovery from durable state.
- Payout failure after FX, returned destination funds, reversal, and
  reconciliation differences: remain unresolved unless evidence and approved
  policy support a distinct, linked, idempotent outcome.
- Institution isolation and ledger/reporting consistency across every flow.
- Exact execution mismatch tests for source principal, destination proceeds, rate,
  fee, executor, beneficiary, currency, and provider references.

Current implementation deliberately rejects unsupported cross-currency or
source/output-mismatched trade requests. These scenarios must continue to fail
closed until the approved fee/pricing model, execution evidence contract,
durable FX posting service, recovery state machine, and Finance mappings exist.

## Engineering defaults and evidence-dependent configuration

These are implementation defaults, not requests for additional policy
approval. Apply them consistently. Only configure the route-specific facts
below when evidence supports them.

| Area | Default to implement | Evidence/configuration still required |
| --- | --- | --- |
| USD ownership and asset classification | Never infer ownership from a provider API status. Use the evidenced custody/cash, provider claim, or customer safeguarded-funds role. Keep production FX off where ownership/control is unknown. | Executed route agreement and independent custody evidence (matrix above). |
| Quote, fee, rounding | Integer minor units; percentage fee rounds up; conversion rounds down; fixed-destination uses the minimum principal meeting the target; all proceeds/surplus belong to the customer and are disclosed. | Ensure customer-facing terms disclose these same semantics before launch. |
| Fee and spread recognition | Keep customer fee and provider fee separate; defer customer fee until verified successful payout; defer contracted principal spread until both FX legs settle; no inventory gain/loss unless owned fungible inventory is evidenced. | Route-specific fee collection and source-cost evidence; no speculative chart mappings. |
| Unknown outcome, failure, refund | No blind create retry, no hold release on unknown, no automatic source-currency make-whole, no automatic compensation. Query/reconcile first; unresolved cases stay open for manual recovery. | None to implement these safe defaults. Any eventual customer action must follow accepted customer terms and verified movement. |
| Account IDs and dimensions | Require explicit accounts with matching currency and owner dimensions; fail closed on missing or conflicting mappings; balance each currency independently. | Actual chart IDs and legal-entity/provider/custody mapping evidence. |
| Simulation | Keep simulator/proof fixtures distinct from provider adapters and production funding; simulated success never becomes financial evidence. | Deployment verification that production does not enable simulator or proof-only funding. |

Independent of these choices, callbacks remain evidence/hints only; uncertain
outcomes remain unresolved; entries are append-only; journals balance per
currency; institution isolation applies; and no distributed transaction or
cross-service database write is permitted. Implement these requirements and
do not reopen them as policy questions.
