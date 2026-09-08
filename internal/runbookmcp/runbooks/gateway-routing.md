# Gateway routing

A real MCP backend is reached at `/<namespace>/<server-name>/mcp`;
Portkeeper removes the routing prefix and forwards to the backend's `/mcp`.
Configure the client to send a Kubernetes ServiceAccount bearer token with
audience `portkeeper` on every request. The gateway verifies it using
TokenReview and requires that ServiceAccount in the server's
`spec.allowedServiceAccounts`. `X-Agent-ID` cannot establish identity.

A 401 means the token is missing, invalid, expired or has the wrong audience.
A 403 means the verified account is not allowed for this server. Client
credentials are stripped before proxying to the backend.

For an unknown-server response, inspect `kubectl get mcpservers -A`.
The gateway refreshes its registry every five seconds. The older
`/<server-name>/mcp` URL works only when that name is unique across namespaces;
an ambiguous legacy URL returns 409. Use the namespaced URL to select a server.

A 503 can mean TokenReview is unavailable, authorization policy is older
than 15 seconds, or the latest observed MCPServer is not ready. Check the
response and gateway logs before investigating the backend. Inspect
`kubectl describe mcpserver -n <namespace> <name>` for its Ready condition,
then inspect the Deployment and pod logs. Readiness requires a completed
current-generation rollout with an available replica and a passing TCP probe.
It does not certify application-level MCP tool behavior.

A 502 means the gateway could not reach a backend that its cache considered
ready. Controller observation and gateway polling are asynchronous, so a
recent failure can produce 502 before the registry reflects it as 503.

When running the gateway on the host, set `GATEWAY_BACKEND_HOST=localhost`
and forward the backend Service's port to the same local port.
