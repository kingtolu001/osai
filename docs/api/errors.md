# Customer API error catalogue

Errors use this envelope:

```json
{
  "error": {
    "code": "VALIDATION_ERROR",
    "message": "A safe description",
    "request_id": "req_..."
  }
}
```

The request identifier also appears in `X-Osai-Correlation-Id` and
`X-Request-Id`. Messages are intended for clients; they must not be used as
machine-readable conditions. Branch on `error.code`.

| HTTP | Code | Meaning |
| --- | --- | --- |
| 400 | `VALIDATION_ERROR` | Missing or malformed request data, including a missing required idempotency key. |
| 400 | `UNSUPPORTED_CORRIDOR` | The requested source or destination currency is not supported by the current quote path. |
| 401 | `UNAUTHENTICATED` | Credentials are missing or invalid. |
| 403 | `CUSTOMER_DISABLED` | The institution is not active or API access is disabled. |
| 403 | `FORBIDDEN` | The institution is not entitled to the operation or cannot access the requested resource. |
| 404 | `NOT_FOUND`, `QUOTE_NOT_FOUND`, `TRADE_NOT_FOUND`, `SETTLEMENT_NOT_FOUND` | The route or institution-scoped resource was not found. |
| 409 | `IDEMPOTENCY_CONFLICT` | The key was already used with a different request. |
| 409 | `IDEMPOTENCY_IN_PROGRESS` | The same request is still being processed. |
| 503 | `SERVICE_UNAVAILABLE`, `READ_SERVICE_UNAVAILABLE` | A required owner service or persistence dependency is unavailable, including the customer authentication service. |

This is the set currently emitted by the gateway, not a claim that all internal
domain errors have a final, versioned public mapping. The final V1 API ADR and
complete error mapping remain outstanding.
