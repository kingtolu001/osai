# Osai Ledger — Money Model Design (v0.2, historical proposal)

> Status: historical engineering proposal; not Finance-approved · Owner: System Architect · Audience: backend engineers, finance/ops
> This document's chart codes, account classifications, debit/credit examples, fee recognition, and reversal economics are illustrative and are not authority for production postings. Do not use them to implement or enable cross-currency FX accounting. Finance/Treasury approval is still required. See [the current FX and ledger policy review](./docs/FX_POSITION_AND_LEDGER_POLICY_REVIEW.md).
> Open decisions include principal vs agency accounting, custody mapping, fee recognition, inventory valuation, and reversal economics.

---

## 1. Money type

```
Money { amountMinor: bigint, currency: Currency }   // currency ALWAYS attached
```

- `bigint` minor units end-to-end. Per-currency scale lives in config (fiat = 2, JPY = 0, KWD = 3, USDC = 6), applied at the adapter edge — never by callers.
- No floats anywhere, including FX rates (decimal + explicit precision) and provider payloads.
- Mixed-currency arithmetic is **unrepresentable** at the type level. FX is always two-legged; each currency leg balances inside the transaction.
- IDs opaque/non-sequential; timestamps UTC; every posting carries `trade_id`, `journal_id`, `correlation_id`, external reference, actor/source.

---

## 2. Design decisions

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | Client money never nets with Osai cash (`1100` vs `1300`); sweeps are explicit entries | Regulatory + trust boundary, not an accounting preference |
| 2 | Holds are real journal entries (`2000 → 2100` reclass) with `balance_holds` row on top for expiry mechanics | Free audit trail; quote expiry = release entry |
| 3 | Suspense (`1400`) is mandatory; only a resolved recon break drains it | Never auto-book unexplained money |
| 4 | Multi-currency txns = linked per-currency legs, each balanced; no contra plugs | Keeps "every journal balances" testable per currency |
| 5 | Failure economics and reversal slippage require an approved policy | Appendix B remains open; no production posting authority |

---

## 3. Chart of accounts

| Code | Account | Class | Normal balance | Why it exists |
|------|---------|-------|----------------|---------------|
| 1100 | `OSAI_OPERATING_CASH · {bank} · {ccy}` | Asset | Debit | Osai's own money. Never netted against client funds; every touch is an explicit sweep entry. |
| 1200 | `PROVIDER_RECEIVABLE · {provider} · {ccy}` | Asset | Debit | Money owed to Osai by an LP, VASP or bank. Drained only by provider evidence, reconciled daily. |
| 1300 | `CLIENT_FUNDS_BANK · {custody_bank} · {ccy}` | Asset | Debit | Client prefunded money held at a partner bank. Off-limits to Osai operations. |
| 1400 | `SUSPENSE · {ccy}` | Asset | Debit | Unexplained money. Only a resolved reconciliation break may drain this account. |
| 2000 | `CLIENT_PREFUND · {institution} · {ccy}` | Liability | Credit | Client's available claim on deposited funds. |
| 2100 | `CLIENT_PREFUND_HELD · {institution} · {ccy}` | Liability | Credit | Reserved at quote acceptance; auto-releases on quote expiry. Mirrors `balance_holds`. |
| 2200 | `CLIENT_SETTLEMENT_PAYABLE · {institution} · {ccy}` | Liability | Credit | Approved payout owed to the client, awaiting final settlement evidence. |
| 2300 | `PROVIDER_PAYABLE · {provider} · {ccy}` | Liability | Credit | Osai owes a provider. Created/increased by credit; reduced/drained by debit when funded or settled. |
| 3100 | `FX_SPREAD_INCOME · {ccy}` | Revenue | Credit | Principal-model spread. Disappears if agency/fee-only model is chosen. |
| 3200 | `FEE_INCOME · {ccy}` | Revenue | Credit | Explicit client fees. |
| 3300 | `PROVIDER_FEE_EXPENSE · {ccy}` | Expense | Debit | LP / VASP / rail fees charged to Osai. |
| 3400 | `REVERSAL_LOSS · {ccy}` | Expense | Debit | Osai-absorbed slippage on failed settlements. Policy decision (Appendix B). |

Account dimensions: owner (`institution` / `provider` / `bank`), currency, status. **One currency per account — enforced by DB constraint.**

---

## 4. Golden scenario journals

Historical illustrative examples only, not canonical or approved posting fixtures. All amounts shown are in minor units as integers.

### 4.1 Scenario A — Golden path

ClientX prefunded ₦1,505,000 (principal ₦1,500,000 + ₦5,000 fee). Quote: ₦1,500/USD all-in. LP rate ₦1,480/USD → ₦20,000 spread. One trade, one beneficiary payout in USD.

| Txn | Trigger | Account | Debit | Credit |
|-----|---------|---------|-------|--------|
| JT-000 | Client prefund received at BankA | 1300 CLIENT_FUNDS_BANK · BankA · NGN | 1,505,000 | |
| | | 2000 CLIENT_PREFUND · ClientX · NGN | | 1,505,000 |
| JT-101 | Quote accepted — hold placed | 2000 CLIENT_PREFUND · ClientX · NGN | 1,505,000 | |
| | | 2100 CLIENT_PREFUND_HELD · ClientX · NGN | | 1,505,000 |
| JT-102 | FX executed with LP1 — NGN leg | 2100 CLIENT_PREFUND_HELD · ClientX · NGN | 1,480,000 | |
| | | 2300 PROVIDER_PAYABLE · LP1 · NGN | | 1,480,000 |
| JT-102 | FX executed with LP1 — USD leg | 1200 PROVIDER_RECEIVABLE · LP1 · USD | 1,000 | |
| | | 2200 CLIENT_SETTLEMENT_PAYABLE · ClientX · USD | | 1,000 |
| JT-103 | NGN funding wire to LP1 (BankA evidence) | 2300 PROVIDER_PAYABLE · LP1 · NGN | 1,480,000 | |
| | | 1300 CLIENT_FUNDS_BANK · BankA · NGN | | 1,480,000 |
| JT-104 | USD credited by LP1 (provider evidence) | 1300 CLIENT_FUNDS_BANK · BankB · USD | 1,000 | |
| | | 1200 PROVIDER_RECEIVABLE · LP1 · USD | | 1,000 |
| JT-105 | Beneficiary payout confirmed at BankB | 2200 CLIENT_SETTLEMENT_PAYABLE · ClientX · USD | 1,000 | |
| | | 1300 CLIENT_FUNDS_BANK · BankB · USD | | 1,000 |
| JT-106 | Trade closed — income recognized, hold released | 2100 CLIENT_PREFUND_HELD · ClientX · NGN | 25,000 | |
| | | 3100 FX_SPREAD_INCOME · NGN | | 20,000 |
| | | 3200 FEE_INCOME · NGN | | 5,000 |
| JT-107 | Income sweep: custody → operating cash | 1100 OSAI_OPERATING_CASH · BankA · NGN | 25,000 | |
| | | 1300 CLIENT_FUNDS_BANK · BankA · NGN | | 25,000 |

Post-conditions: HELD = 0 · PREFUND = 0 · custody NGN = 0 · custody USD = 0 · Osai cash +25,000 = income recognized. Client received USD 1,000.

### 4.2 Scenario B — Payout fails after FX executed

Entering state: JT-104 done; USD 1,000 sits at BankB; payout fails after retries. Policy: Osai reverses the FX and makes the client whole at ₦1,505,000. LP buys back USD at ₦1,470 → Osai books ₦10,000 reversal loss and injects the cash.

| Txn | Trigger | Account | Debit | Credit |
|-----|---------|---------|-------|--------|
| JT-201 | Reverse FX — USD leg | 2200 CLIENT_SETTLEMENT_PAYABLE · ClientX · USD | 1,000 | |
| | | 1300 CLIENT_FUNDS_BANK · BankB · USD | | 1,000 |
| JT-201 | Reverse FX — NGN leg | 1200 PROVIDER_RECEIVABLE · LP1 · NGN | 1,470,000 | |
| | | 2100 CLIENT_PREFUND_HELD · ClientX · NGN | | 1,470,000 |
| JT-202 | Make-whole top-up (1,470,000 + 25,000 residue + 10,000 Osai = 1,505,000) | 3400 REVERSAL_LOSS · NGN | 10,000 | |
| | | 2100 CLIENT_PREFUND_HELD · ClientX · NGN | | 10,000 |
| JT-203 | Reversal proceeds received from LP1 | 1300 CLIENT_FUNDS_BANK · BankA · NGN | 1,470,000 | |
| | | 1200 PROVIDER_RECEIVABLE · LP1 · NGN | | 1,470,000 |
| JT-204 | Release hold back to client prefund | 2100 CLIENT_PREFUND_HELD · ClientX · NGN | 1,505,000 | |
| | | 2000 CLIENT_PREFUND · ClientX · NGN | | 1,505,000 |
| JT-205 | Osai injects the custody shortfall | 1300 CLIENT_FUNDS_BANK · BankA · NGN | 10,000 | |
| | | 1100 OSAI_OPERATING_CASH · BankA · NGN | | 10,000 |

Post-conditions: client whole at ₦1,505,000 · Osai P&L −10,000, cash −10,000 · no orphan balances.

### 4.3 Scenario C — Suspense break

BankA statement shows ₦1,505,000 in with no matching reference. Never auto-booked to a client.

| Txn | Trigger | Account | Debit | Credit |
|-----|---------|---------|-------|--------|
| JT-301 | Unmatched inbound funds isolated | 1300 CLIENT_FUNDS_BANK · BankA · NGN | 1,505,000 | |
| | | 1400 SUSPENSE · NGN | | 1,505,000 |
| JT-302 | Recon break resolved — matched to ClientX deposit | 1400 SUSPENSE · NGN | 1,505,000 | |
| | | 2000 CLIENT_PREFUND · ClientX · NGN | | 1,505,000 |

---

## 5. Invariants these fixtures enforce

1. **Per-currency balance** — every journal transaction balances debits and credits to zero per currency (DB constraint + property test).
2. **Non-negative spendability** — an institution cannot reserve or spend more than its available funds. A hold may only be created when `new_hold_amount <= available_before_hold`.
3. **External-reference uniqueness** — the same external reference (bank statement line, provider transfer ID) books exactly once, enforced by a unique constraint on `(source, external_ref)`.
4. **Hold reclassification conserves client liability** — moving value from `2000 CLIENT_PREFUND` to `2100 CLIENT_PREFUND_HELD` changes spendability but does not change the total amount Osai owes the client. A hold must never create additional client liability.
5. **Total-client-liability safeguarding** — per currency, customer solvency/backing is measured against total client liabilities, not merely available balances:

   ```text
   Σ client liability balances
   <=
   Σ eligible safeguarded custody assets
   + explicitly-recognised client settlement receivables
   - explicitly-recognised client settlement payables/shortfalls
   ```

   Client liabilities include available, held/reserved, and other client-owned in-flight buckets. A breach is a financial-integrity failure: new money movement fails closed.
6. **Spendability is not solvency** — aggregate institution available balances are an operational/customer-availability view only and must never be used as the safeguarding measure.
7. **No over-release** — a hold can only be released or consumed once, and `2000/2100/2200` client-liability buckets must never be over-released below their valid economic balance.
8. **Replay safety** — replay of any already-successful command produces the same economic outcome and no additional journal effect.
9. **Deterministic rebuild** — rebuilding from the immutable journal reproduces ledger balances deterministically.

---

## 6. Canonical account and position model

This ledger chart is the accounting layer underneath the first-class account model. The application account model is intentionally separate from ledger account codes.

### 6.1 First-class account types

| Account type | Owner | Ledger mapping | V1 meaning |
|---|---|---|---|
| `INSTITUTION_SETTLEMENT` | Institution | `2000 CLIENT_PREFUND` plus `2100 CLIENT_PREFUND_HELD` views | Institution settlement position for one currency. Usable balance is derived from journal truth and active holds. |
| `OMNIBUS_SAFEGUARDED` | Osai, for clients | `1300 CLIENT_FUNDS_BANK` per custody bank + currency | Pooled client funds held at a partner/custody bank, structurally segregated from Osai operating cash. |
| `OSAI_OPERATING` | Osai | `1100 OSAI_OPERATING_CASH` | Osai house money: capital, income sweeps, reversal top-ups, operating cash. Never netted with client money. |
| `NOSTRO_PROVIDER` | Provider/external counterparty | `1200 PROVIDER_RECEIVABLE` and `2300 PROVIDER_PAYABLE` per provider + currency | Osai's internal view of money due from/to each provider, reconciled against external provider evidence. |
| `TREASURY_INVENTORY` | Osai | reserved `1500` series | Reserved schema hook only. Unused in V1 while trades close flat within each trade. |
| `FEE_AND_INCOME` | Osai | `3100`, `3200`, `3300`, `3400` | Explicit P&L accounts for spread, fees, provider costs and reversal loss. |
| `SUSPENSE` | System | `1400 SUSPENSE` | Unexplained money. Only an approved reconciliation resolution may drain it. |

### 6.2 Application account schema

```sql
accounts (
  id,
  owner_type,     -- institution | osai | provider | system
  owner_id,
  currency,
  account_type,
  structure,      -- omnibus | safeguarded | designated
  status,
  opened_at
)
```

Rules:

- one currency per account;
- no writable `balance` column is financial truth;
- balances and positions are derived from the journal;
- `structure` describes the bank-layer custody mapping and can evolve without changing the chart of accounts;
- external beneficiaries are not fabricated as Osai accounts.

### 6.3 Position derivation

| Position | Definition | Source of truth |
|---|---|---|
| Ledger balance | Sum of journal entries on the mapped ledger accounts | Immutable journal |
| Available | Journal-derived institution position minus active holds and policy reserves | Derived, never stored as truth |
| Reserved | Sum of active holds bound to in-flight obligations | `balance_holds` + obligations |
| External / nostro position | `1200 - 2300` per provider + currency | Internal journal, reconciled to provider-reported evidence |

### 6.4 Custody structure

V1 uses safeguarded omnibus client money at partner banks:

- `1300 CLIENT_FUNDS_BANK` is pooled per custody bank + currency;
- client ownership is represented by institution-specific liability accounts (`2000/2100/2200`);
- `1100 OSAI_OPERATING_CASH` remains structurally separate;
- if a partner later provides designated/per-institution sub-accounts, the application `accounts.structure` and dimensions can represent that without changing the ledger principles.

### 6.5 Architecture separation

- **Ledger** answers: what money exists where.
- **State machines** answer: what operation is happening.
- **Provider evidence** answers: what the outside world claims happened.
- **Reconciliation** compares those truths and resolves disagreements through suspense and approved adjustments.

Provider evidence may advance a guarded state transition, but it never writes balances directly.

---

## 7. Explicitly out of scope for V1

- Credit lines / receivable-from-client accounts (funding model TBD).
- Client-visible stablecoin balances (rail-only per Section 12 of the handoff).
- Net settlement / multi-trade netting.
- FX inventory valuation, cost basis, and realized gains/losses; these remain proposals pending Finance/Treasury approval. See the current FX and ledger policy review.

*One instruction in. One financial truth inside. Many rails underneath. One provable settlement outcome out.*
