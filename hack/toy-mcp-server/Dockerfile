FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -o /toy-mcp-server .

FROM gcr.io/distroless/static-debian12
COPY --from=build /toy-mcp-server /toy-mcp-server
EXPOSE 9000
ENTRYPOINT ["/toy-mcp-server"]
