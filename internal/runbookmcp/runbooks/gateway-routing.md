# Gateway routing

A real MCP backend is reached at `/<server-name>/mcp`; Portkeeper removes
the server prefix and forwards the request to `/mcp` on the backend.
Configure the client to send `X-Agent-ID` on every request. This header
is an attribution label, not authenticated identity.

For an unknown-server response, inspect `kubectl get mcpservers -A`.
The gateway refreshes its registry every five seconds. Server names must
currently be unique across namespaces because the registry uses name-only
keys.

For a bad-gateway response, inspect the backend Deployment, Service, and
pod logs. A CR's current `Ready` phase only means resources were reconciled;
it does not yet prove the workload is available.

When running the gateway on the host, set `GATEWAY_BACKEND_HOST=localhost`
and forward the backend Service's port to the same local port.
