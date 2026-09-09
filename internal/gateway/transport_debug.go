package gateway

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"runtime/debug"
)

func diagnosticTransport() http.RoundTripper {
	if os.Getenv("GATEWAY_DEBUG_TRANSPORT") != "1" {
		return nil
	}
	standard, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		panic("transport diagnostics require the standard HTTP transport")
	}
	transport := standard.Clone()
	dial := transport.DialContext
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &diagnosticConn{Conn: conn}, nil
	}
	return transport
}

type diagnosticConn struct {
	net.Conn
}

func (c *diagnosticConn) Close() error {
	log.Printf("diagnostic_backend_close local=%s remote=%s stack:\n%s", c.LocalAddr(), c.RemoteAddr(), debug.Stack())
	return c.Conn.Close()
}
