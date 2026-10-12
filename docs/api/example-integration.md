# Example V1 integration

The example uses the API key shown by the credential provisioning process. Keep
the secret in a secret manager and never print it in CI logs.

```sh
export OSAI_API_BASE_URL="https://api.example.invalid"
export OSAI_CLIENT_ID="ck_..."
export OSAI_CLIENT_SECRET="..."
export OSAI_AUTH="Authorization: ApiKey ${OSAI_CLIENT_ID}:${OSAI_CLIENT_SECRET}"
```

Create a quote with a stable idempotency key:

```sh
curl --fail-with-body -X POST "${OSAI_API_BASE_URL}/v1/quotes" \
  -H "${OSAI_AUTH}" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: quote-order-184" \
  -H "X-Osai-Correlation-Id: order-184" \
  -d '{"base_amount_minor":10000,"base_currency":"NGN","quote_currency":"USD","destination_rail":"sim_lp_1","urgency":"normal"}'
```

Save the returned `quote_id`. Accept the quote with the same key if retrying the
same acceptance:

```sh
curl --fail-with-body -X POST "${OSAI_API_BASE_URL}/v1/quotes/${QUOTE_ID}/accept" \
  -H "${OSAI_AUTH}" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: accept-order-184" \
  -H "X-Osai-Correlation-Id: order-184" \
  -d "{\"quote_id\":\"${QUOTE_ID}\"}"
```

Use the returned `trade_id` to retrieve canonical trade state:

```sh
curl --fail-with-body "${OSAI_API_BASE_URL}/v1/trades/${TRADE_ID}" \
  -H "${OSAI_AUTH}" \
  -H "X-Osai-Correlation-Id: order-184"
```

The API currently exposes single-trade retrieval, not the trade-list endpoint.
Do not treat a timeout as proof that a money-moving request failed; retry with
the same idempotency key and identical body.
