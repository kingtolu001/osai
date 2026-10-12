# Idempotency

Send an `Idempotency-Key` for state-creating customer requests. It is required
for:

- `POST /v1/quotes`
- `POST /v1/quotes/{quote_id}/accept`
- `POST /v1/beneficiaries`
- `POST /v1/webhook-endpoints`
- `POST /v1/webhook-deliveries/{event_id}/replay`

Keys are scoped by institution and operation. A key should be unique for one
logical request and retained by the client when retrying that request.

| Request | Result |
| --- | --- |
| New key | The gateway reserves the key and executes the operation. |
| Same key and same canonical request | The original HTTP status and response body are replayed. No new effect is requested. |
| Same key and different request | HTTP `409`, code `IDEMPOTENCY_CONFLICT`. |
| Same key while another request holds the reservation | HTTP `409`, code `IDEMPOTENCY_IN_PROGRESS`; retry with backoff. |

The gateway stores a SHA-256 request fingerprint and the completed response in
its PostgreSQL-owned idempotency table. The request payload, including webhook
signing secrets, is not stored in that table. Business services still enforce
their own idempotency and uniqueness rules.

If a downstream write succeeds but the gateway crashes before recording the
response, retry only with the same key and same request. Webhook configuration
is an owner-local upsert, so recovery may repeat configuration without
creating a second endpoint record. Do not use a new key to recover an uncertain
request.

Webhook replay also records the idempotency key in Notification/Webhook,
alongside the gateway response reservation. This owner-side record preserves
same-event replay idempotency if the gateway or webhook service restarts.
