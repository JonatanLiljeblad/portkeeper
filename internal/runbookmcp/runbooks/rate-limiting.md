# Rate limiting

Portkeeper authenticates a ServiceAccount bearer token and applies a token
bucket per verified ServiceAccount username
across all server routes. The default is five HTTP requests per second,
with a burst of ten.

Configure positive `GATEWAY_RATE_LIMIT_RPS` and
`GATEWAY_RATE_LIMIT_BURST` values when starting the gateway. An HTTP 429
means the current agent's request budget is exhausted or the gateway has
reached its 10,000-identity limiter capacity.

The limit counts transport requests, including MCP initialization and
notifications, not just tool invocations. Buckets live in one gateway
process and reset on restart. Replicas do not share budgets. At capacity,
only buckets idle for at least 15 minutes with fully replenished budgets
are evicted; active or depleted buckets cannot be reset by creating new IDs.

Changing `X-Agent-ID` cannot change the authenticated identity or quota.
TokenReview is required on every HTTP request; already-open streams are not
terminated when the token expires.
