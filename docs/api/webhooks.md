# Customer webhooks

Customer webhook configuration and outbound delivery are owned by the
Notification/Webhook service. Configure the destination through
`POST /v1/webhook-endpoints`; the institution must have the `webhooks:write`
entitlement and the request must include an `Idempotency-Key`.

Example request:

```json
{
  "endpoint_url": "https://partner.example/webhooks/osai",
  "signing_secret": "a-client-generated-random-secret",
  "subscribed_event_types": ["quote.accepted"]
}
```

The signing secret is write-only in the API response. The Notification service
stores it encrypted. Outside local proof mode, destinations must use HTTPS and
resolve only to publicly routable IP addresses. DNS is checked again when
connecting, connections are pinned to the checked address, and redirects are
not followed. Private and HTTP targets are permitted only when
`OSAI_LOCAL_PROOF_MODE=true` and `OSAI_ENVIRONMENT` is not `prod` or
`production`.

## Signature verification

Each delivery is a JSON `POST`. The signature is lowercase hexadecimal
HMAC-SHA256 over `event_id + "." + timestamp + "." + raw_request_body`, keyed
by the configured signing secret. This binds the stable deduplication ID to the
signed body.

| Header | Meaning |
| --- | --- |
| `X-Osai-Timestamp` | UTC RFC3339 timestamp included in the signature input. |
| `X-Osai-Signature` | HMAC-SHA256 signature of the timestamp and exact raw body. |
| `X-Osai-Event-Id` | Stable logical event ID; use it for receiver-side deduplication. |
| `X-Osai-Event-Type` | Event type. |
| `X-Osai-Event-Version` | Event schema version. |
| `X-Osai-Correlation-Id` | Correlation identifier for the originating operation. |

The JSON envelope includes `event_id`, `event_type`, `event_version`,
`correlation_id`, `payload`, and `occurred_at`. Verify the signature against the
unmodified raw body before parsing it. Reject timestamps outside a five-minute
clock-skew window and persist processed event IDs so retries do not repeat
business effects.

## Delivery and recovery

Events, endpoint configuration, and delivery attempts are persisted by the
Notification service. Delivery is at-least-once. Successful 2xx responses are
recorded as success; 429 and 5xx responses are retryable; other non-2xx
responses are treated as terminal failures. Network errors are retryable.
Retries use exponential scheduling and the configured attempt limit; exhausted
or terminal failures are marked dead-letter. The worker recovers due rows after
restart. If a stored destination fails delivery-time security/configuration
validation, the worker records a terminal failed attempt, marks it dead-letter,
and logs a safe failure message rather than retrying it indefinitely.

Replay a terminal delivery using
`POST /v1/webhook-deliveries/{event_id}/replay`, with the institution's
`webhooks:write` entitlement and a fresh `Idempotency-Key`. The request body is
empty. A `202` response means the same immutable event ID has been queued; it
does not create a new domain event. Only successful or dead-lettered deliveries
can start a replay. Concurrent/in-progress replays return the existing queued
state, and delivery attempts remain in the durable history.

The Technical Handoff lists the minimum customer-relevant events:
`quote.expired`, `trade.accepted`, `trade.funding_required`, `trade.funded`,
`trade.fx_executed`, `trade.settlement_processing`, `trade.settled`,
`trade.failed`, `trade.reversed`, and customer-relevant
`reconciliation.break_created`. The implementation currently exercises only a
subset; each event must be published from its authoritative domain transition.
