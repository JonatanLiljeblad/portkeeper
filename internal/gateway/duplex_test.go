package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

func TestProxyFlushDoesNotCloseRequestBody(t *testing.T) {
	for _, protocol := range []string{"http1", "http2"} {
		t.Run(protocol, func(t *testing.T) {
			testProxyFlushDoesNotCloseRequestBody(t, protocol == "http2")
		})
	}
}

func testProxyFlushDoesNotCloseRequestBody(t *testing.T, http2 bool) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	flushed := make(chan struct{})
	bodyRead := make(chan error, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: progress\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
			return
		}
		select {
		case err := <-bodyRead:
			if !errors.Is(err, io.EOF) {
				t.Errorf("request body's final read after response flush = %v, want EOF", err)
			}
		case <-ctx.Done():
			t.Error("transport did not finish reading the request")
			return
		}
		io.WriteString(w, "data: complete\n\n")
	}))
	defer backend.Close()
	reg := &Registry{backends: map[types.NamespacedName]Backend{
		{Namespace: "test", Name: "demo"}: {
			Namespace: "test", Name: "demo", Ready: true,
			Address:                strings.TrimPrefix(backend.URL, "http://"),
			AllowedServiceAccounts: testAccounts, PolicyObservedAt: time.Now(),
		},
	}}
	router := NewRouter(reg, NewAgentLimiter(100, 100), testAuthenticator(t))
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.ProtoMajor == 2) != http2 {
			t.Errorf("unexpected protocol: %s", r.Proto)
		}
		r.Body = &flushGatedBody{
			ReadCloser: r.Body, remaining: r.ContentLength, ctx: ctx,
			flushed: flushed, observed: bodyRead,
		}
		router.ServeHTTP(&flushSignalingWriter{ResponseWriter: w, flushed: flushed}, r)
	}))
	server.EnableHTTP2 = http2
	if http2 {
		server.StartTLS()
	} else {
		server.Start()
	}
	defer server.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/test/demo/mcp", strings.NewReader(`{"method":"tools/call"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != "data: progress\n\ndata: complete\n\n" {
		t.Fatalf("stream status=%d body=%q error=%v", resp.StatusCode, body, err)
	}
}

func TestProxyRequiresHTTP1FullDuplexSupport(t *testing.T) {
	reg := &Registry{backends: map[types.NamespacedName]Backend{
		{Namespace: "test", Name: "demo"}: {
			Namespace: "test", Name: "demo", Ready: true, Address: "127.0.0.1:1",
			AllowedServiceAccounts: testAccounts, PolicyObservedAt: time.Now(),
		},
	}}
	router := NewRouter(reg, NewAgentLimiter(100, 100), testAuthenticator(t))
	req := httptest.NewRequest(http.MethodPost, "/test/demo/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer test-token")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusInternalServerError || !strings.Contains(recorder.Body.String(), "streaming request transport unavailable") {
		t.Fatalf("unsupported full duplex was ignored: %d %s", recorder.Code, recorder.Body)
	}
}

// Delay the transport's excess-body check until the first SSE flush. Without
// full duplex, net/http closes the inbound body during that flush.
type flushGatedBody struct {
	io.ReadCloser
	remaining int64
	ctx       context.Context
	flushed   <-chan struct{}
	observed  chan<- error
	once      sync.Once
}

func (b *flushGatedBody) Read(p []byte) (int, error) {
	final := b.remaining == 0
	if final {
		select {
		case <-b.flushed:
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		}
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	if final {
		b.once.Do(func() { b.observed <- err })
	}
	return n, err
}

type flushSignalingWriter struct {
	http.ResponseWriter
	flushed chan struct{}
	once    sync.Once
}

func (w *flushSignalingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *flushSignalingWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	w.once.Do(func() { close(w.flushed) })
	return err
}
