FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/controller ./cmd/controller && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/runbooks ./hack/runbook-mcp-server && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/demo-client ./hack/mcp-client

FROM scratch AS runtime
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 65532:65532
ENTRYPOINT ["/portkeeper"]

FROM runtime AS controller
COPY --from=build /out/controller /portkeeper

FROM runtime AS gateway
COPY --from=build /out/gateway /portkeeper

FROM runtime AS runbooks
ENV RUNBOOK_ADDR=:9001
COPY --from=build /out/runbooks /portkeeper

FROM runtime AS demo-client
COPY --from=build /out/demo-client /portkeeper
