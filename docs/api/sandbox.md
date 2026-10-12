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

Trade listing, settlement reports, per-process rate limits, and the
environment-scoped credential lifecycle are implemented. The Phase 5 partner
API remains incomplete: provider status and the agreed source-funding/FX
execution behavior are not implemented, webhook event publication is partial,
and the full public flow has not been re-run against independent services in
this environment. See the Phase 5 handoff and readiness assessment before
treating this local proof as partner acceptance.
