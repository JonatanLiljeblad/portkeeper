# Rate limiting

Portkeeper requires `X-Agent-ID` and applies a token bucket per agent ID
across all server routes. The default is five HTTP requests per second,
with a burst of ten.

Configure positive `GATEWAY_RATE_LIMIT_RPS` and
`GATEWAY_RATE_LIMIT_BURST` values when starting the gateway. An HTTP 429
means the current agent's request budget is exhausted.

The limit counts transport requests, including MCP initialization and
notifications, not just tool invocations. Buckets live in one gateway
process, reset on restart, and are not currently evicted.

Self-reported IDs are not a security boundary. Do not claim enforced
identity-based quotas until authenticated identity is implemented.
