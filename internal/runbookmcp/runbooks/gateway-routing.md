# Gateway routing

A real MCP backend is reached at `/<namespace>/<server-name>/mcp`;
Portkeeper removes the routing prefix and forwards to the backend's `/mcp`.
Configure the client to send `X-Agent-ID` on every request. This header
is an attribution label, not authenticated identity.

For an unknown-server response, inspect `kubectl get mcpservers -A`.
The gateway refreshes its registry every five seconds. The older
`/<server-name>/mcp` URL works only when that name is unique across namespaces;
an ambiguous legacy URL returns 409. Use the namespaced URL to select a server.

A 503 means the latest observed MCPServer is not ready. Inspect
`kubectl describe mcpserver -n <namespace> <name>` for its Ready condition,
then inspect the Deployment and pod logs. Readiness requires a completed
current-generation rollout with an available replica and a passing TCP probe.
It does not certify application-level MCP tool behavior.

A 502 means the gateway could not reach a backend that its cache considered
ready. Controller observation and gateway polling are asynchronous, so a
recent failure can produce 502 before the registry reflects it as 503.

When running the gateway on the host, set `GATEWAY_BACKEND_HOST=localhost`
and forward the backend Service's port to the same local port.
