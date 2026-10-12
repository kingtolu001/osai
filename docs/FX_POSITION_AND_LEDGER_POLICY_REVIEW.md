# FX Position and Ledger Policy Review

**Status: engineering proposal for Finance, Treasury, Legal/Compliance, and
operations review. It is not formal accounting approval or authority to post
FX entries.**

The product and architecture baseline below is approved for engineering
constraints: V1 targets NGN/USD, Osai controls commercial pricing and treasury
exposure, and USD capacity is pre-funded. The specific chart-of-accounts
mappings, debit/credit directions, valuation and fee-recognition policies,
custody classification, and reversal treatment are not approved. Production
cross-currency execution and posting must remain disabled until those approvals
and the durable Ledger implementation are complete.

This proposal assigns accounting **roles** and evidence/state requirements. It
intentionally assigns no account IDs, posting sides, balancing accounts, or
recognition entries. The examples in
[the historical ledger money-model document](../osai_ledger_money_model_v0.2.md)
are illustrative only and are not approval.

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

## Required accounting roles

These are semantic roles, not chart-of-accounts names or classifications.
Finance must determine the final account IDs, owner dimensions, balance-sheet
classification, and posting side for each role.

| Role | Currency / owner dimensions | Purpose and boundary |
| --- | --- | --- |
| Customer available obligation | NGN or USD; institution | Amount Osai owes the customer and which remains spendable. Derived from the Ledger. |
| Customer reserved obligation | NGN or USD; institution and trade | Customer funds reserved for an accepted, funded operation. A hold is not revenue or a reduction in total customer liability. |
| Source-currency settlement / treasury clearing | NGN; actual counterparty, custodian, or legal entity | Tracks the source leg only when supported by the actual funding and counterparty arrangement. Its asset/liability classification is unresolved. |
| Treasury inventory or provider claim | USD; legal entity and actual custodian/provider | Represents pre-funded USD only where evidence establishes where it is held and who owes it. Do not assume every arrangement is cash or a receivable. |
| Customer destination payout obligation | USD; institution and trade | Tracks the contracted destination amount separately from the NGN source principal and fees. |
| Payout rail / provider clearing | Payout currency; provider and settlement | Tracks the movement to or from the payout provider. Clearing is not proof that the beneficiary received funds. |
| Customer fee tracking | Quote currency and institution/trade | Records the accepted fee calculation as a separate economic component. Revenue recognition timing and destination account are not approved. |
| Provider cost tracking | Actual fee currency and provider | Tracks provider-charged fees separately from customer fees and FX principal; expense classification and timing require approval. |
| Treasury FX result | Approved functional/reporting currency and inventory pool | Separately reports realized trading results from customer fees and provider costs. Valuation and recognition policy require approval. |
| Unresolved reconciliation difference | Evidence currency, provider/custodian, and break | Durable break record for investigation. It must not be silently assigned to a customer, fee, or P&L role. Any suspense account mapping and resolution posting need approval. |

## Currency-specific journal template catalogue

The following templates describe which facts and roles belong to a posting
boundary. They do not prescribe debits, credits, account IDs, or missing
balancing entries. No template is postable until Finance approves its complete
mapping and the Ledger validates it transactionally.

| Template | Currency leg(s) | Roles in scope | Required evidence / posting boundary |
| --- | --- | --- | --- |
| Customer prefund | One source currency, e.g. NGN | Custody/funding position; customer available obligation | Independently matched bank/custodian movement, exact currency and amount, institution allocation, and unique evidence reference. |
| Quote acceptance / reservation | NGN for the NGN/USD V1 source leg | Customer available obligation; customer reserved obligation | Immutable accepted quote, expiry, fee breakdown and total source debit. Only a hold/reclassification is eligible; no FX completion or fee revenue. |
| FX execution — source leg | NGN only | Customer reserved obligation; approved source clearing/treasury role; fee tracking only if separately supported | Verified executor, exact NGN principal/debit, actual rate and execution reference, and source settlement evidence. The source-currency journal must balance independently. |
| FX execution — destination leg | USD only | Treasury inventory/provider claim; customer destination payout obligation | Verified USD proceeds, actual applied rate, executor reference, custody/claim basis, and corresponding source trade identity. The USD journal must balance independently. |
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
- Exact execution mismatch tests for source debit, destination proceeds, rate,
  fee, executor, beneficiary, currency, and provider references.

Current implementation deliberately rejects unsupported cross-currency or
source/output-mismatched trade requests. These scenarios must continue to fail
closed until the approved fee/pricing model, execution evidence contract,
durable FX posting service, recovery state machine, and Finance mappings exist.

## Formal approvals still required

1. **Finance / Controller:** final chart-of-accounts IDs and dimensions; debit
   and credit sides; source and destination asset/liability classification;
   customer-fee recognition; provider-fee classification; functional currency;
   inventory/cost-basis method and rounding; realized/unrealized FX gains and
   losses; reversals, failed payouts, make-whole decisions, suspense, and
   reconciliation adjustments.
2. **Treasury:** actual prefunding ownership and custody arrangements; FX
   executor/counterparty; USD inventory boundaries and funding limits; price
   sources; settlement cutoffs; evidence sources; counterparty exposure,
   operational reconciliation, and exception controls.
3. **Legal / Compliance:** required permissions for the product and corridors;
   customer disclosures; safeguarding and segregation treatment; cross-border
   movement and beneficiary requirements; retention and complaint handling.
4. **Security / Operations:** production secrets and identities; maker/checker
   authority; reconciliation and incident runbooks; backup/restore and rebuild
   drills; monitoring and escalation ownership.

Approval of the product and engineering constraints in this review does not
constitute these approvals. Until they are recorded and the implementation
gates pass, production FX execution, FX journals, FX gain/loss recognition, and
post-FX payout recovery remain blocked.
