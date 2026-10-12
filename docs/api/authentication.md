# Customer API authentication

The current V1 HTTP API authenticates institutional clients with an API key:

```http
Authorization: ApiKey <public_id>:<secret>
```

Credentials are issued and validated by the Customer/KYB service. The secret is
returned only at credential creation and is stored as a hash. Do not include
credentials in URLs, source control, support tickets, or logs. Send API calls
over HTTPS outside a local proof environment.

The gateway propagates a request correlation value in
`X-Osai-Correlation-Id` and `X-Request-Id`. Clients may send
`X-Osai-Correlation-Id` or `X-Request-Id`; otherwise the gateway creates one.
The selected value is returned in both response headers and the JSON envelope's
`request_id`.

Set `OSAI_API_ENVIRONMENT` to `sandbox` or `production` for every gateway
process. Startup fails when it is missing or invalid, and the gateway rejects
credentials whose Customer/KYB-owned environment does not match. Sandbox
credentials therefore cannot authenticate against a production gateway (or
vice versa).

## Authorization

Requests are scoped to the institution resolved from the credential. Webhook
configuration additionally requires the `webhooks:write` institution
entitlement. Quote creation and acceptance require `quotes:write`; quote reads
require `quotes:read`. Domain services remain responsible for their own
business and institution checks. Other route-specific entitlement policy is
not yet complete.

## Credential lifecycle

Institutions with `credentials:write` may use:

- `POST /v1/credentials` to create a credential. `label` is required;
  `environment` is `sandbox` or `production` and defaults to `sandbox`.
- `POST /v1/credentials/{public_id}/rotate` to replace an active credential's
  secret. The old secret stops working immediately; there is no overlap window.
- `DELETE /v1/credentials/{public_id}` to revoke a credential.

Institutions with `credentials:read` may use `GET /v1/credentials` and
`GET /v1/credentials/{public_id}`. Read responses contain metadata only. A
secret is returned only by creation or rotation and is not recoverable later.
Credential data is owned by Customer/KYB and persists in its database.

## Rate limits

Authenticated requests are subject to independent per-institution and
per-credential token buckets. Each bucket currently allows 100 requests per
minute with a burst of 10. A rejected request returns HTTP `429`,
`RATE_LIMIT_EXCEEDED`, and `Retry-After: 60`. Limits are process-local and
therefore are not aggregated across multiple gateway replicas; distributed
rate limiting remains a deployment-readiness gap.

## Remaining authentication work

Credentials are environment-scoped, persisted, and can be created, rotated,
and revoked through Customer/KYB gRPC. Independent-service restart verification
for the public credential API, distributed rate limiting, service-to-service
identity, and mTLS/private-connectivity controls remain outstanding. Do not
interpret the implemented local controls as production certification.
