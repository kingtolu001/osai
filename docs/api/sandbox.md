# Local sandbox

The independent-services proof starts the Customer/KYB, Ledger, Notification,
Settlement, Trade, Quote, Reporting, API Gateway, and Temporal worker processes
against a loopback provider simulator and an isolated PostgreSQL database.

Prerequisites:

- PostgreSQL reachable using `OSAI_POSTGRES_DSN` (the local integration
  environment convention uses port `5433`).
- Temporal reachable using `OSAI_TEMPORAL_ADDR` (the local convention uses
  `localhost:7233`).
- Set `OSAI_API_ENVIRONMENT=sandbox` for the API Gateway process; startup
  requires `sandbox` or `production`, and credentials must match the gateway
  environment.
- Sufficient free disk space for Go to compile and stage the service binaries.

Run the proof from the repository root:

```powershell
$env:OSAI_POSTGRES_DSN = "postgres://osai:osai@localhost:5433/osai?sslmode=disable"
$env:OSAI_TEMPORAL_ADDR = "localhost:7233"
go test -p 1 ./integration -run '^TestProviderSandboxSeparateServices$' -count=1 -v
```

This test uses a loopback fake provider and does not prove a real Flutterwave
sandbox transaction. The `OSAI_LOCAL_PROOF_MODE` setting is restricted to
local-proof use and must not be enabled in production. Never place real
provider credentials in test output or source files.

Trade listing, settlement reports, per-process rate limits, the
environment-scoped credential lifecycle, and the entitled
`GET /v1/providers/status` route are implemented. The provider-status route
returns stable customer-facing route IDs, normalized availability, and
capabilities; it does not expose provider identities or internal health
details. Customer event publication is durable for quote expiry and the trade
acceptance/funding/settlement-processing/confirmed transitions. The full
catalog and public flow are not yet proven in this environment: FX execution,
source-currency fee funding/accounting, reversal and reconciliation customer
events, and an independent-services run covering the new provider-status route
remain outstanding. See the Phase 5 handoff and readiness assessment before
treating this local proof as partner acceptance.
