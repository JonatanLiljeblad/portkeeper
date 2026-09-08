package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestMetricsNamespaceLabels(t *testing.T) {
	for _, namespace := range []string{"metrics-one", "metrics-two"} {
		counter := httpRequestsTotal.WithLabelValues(namespace, "same", "mcp", "200", "complete")
		before := &dto.Metric{}
		if err := counter.Write(before); err != nil {
			t.Fatal(err)
		}
		recordRequest(namespace, "same", "mcp", 200, true, time.Millisecond)
		after := &dto.Metric{}
		if err := counter.Write(after); err != nil {
			t.Fatal(err)
		}
		if after.GetCounter().GetValue() != before.GetCounter().GetValue()+1 {
			t.Fatalf("counter did not advance for %s", namespace)
		}
		found := false
		for _, label := range after.Label {
			if label.GetName() == "namespace" && label.GetValue() == namespace {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing namespace label: %v", after.Label)
		}
	}
}

func TestMetricLabelAdmissionIsBounded(t *testing.T) {
	labels := boundedLabels{limit: 2, values: make(map[string]struct{})}
	for _, value := range []string{"team/a", "team/b", "team/a"} {
		if !labels.admit(value) {
			t.Fatalf("rejected admitted label %s", value)
		}
	}
	for i := range 1000 {
		if labels.admit(fmt.Sprintf("team/extra-%d", i)) {
			t.Fatal("label cap exceeded")
		}
	}
	if len(labels.values) != 2 || observedEndpoint("arbitrary-tool") != "other" || observedEndpoint("mcp") != "mcp" {
		t.Fatal("unbounded label mapping")
	}
}

func TestUnresolvedRequestsShareMetricLabels(t *testing.T) {
	counter := httpRequestsTotal.WithLabelValues("", "_unresolved", "other", "404", "complete")
	before := &dto.Metric{}
	if err := counter.Write(before); err != nil {
		t.Fatal(err)
	}
	router := NewRouter(&Registry{}, NewAgentLimiter(1000, 1000), testAuthenticator(t))
	for i := range 100 {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/namespace-%d/server-%d/tool-%d", i, i, i), nil)
		req.Header.Set("Authorization", "Bearer test-token")
		router.ServeHTTP(httptest.NewRecorder(), req)
	}
	after := &dto.Metric{}
	if err := counter.Write(after); err != nil {
		t.Fatal(err)
	}
	if after.GetCounter().GetValue()-before.GetCounter().GetValue() != 100 {
		t.Fatal("unresolved requests used attacker-controlled labels")
	}
}

func TestTruncatedResponseIsRecordedAsAborted(t *testing.T) {
	counter := httpRequestsTotal.WithLabelValues("test", "demo", "mcp", "200", "aborted")
	before := &dto.Metric{}
	if err := counter.Write(before); err != nil {
		t.Fatal(err)
	}
	server := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
		_ = http.NewResponseController(w).Flush()
	}))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/test/demo/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := server.Client().Do(req)
	if err == nil {
		_, err = io.ReadAll(res.Body)
		res.Body.Close()
	}
	if err == nil {
		t.Fatal("truncated response appeared successful")
	}
	after := &dto.Metric{}
	if err := counter.Write(after); err != nil {
		t.Fatal(err)
	}
	if after.GetCounter().GetValue()-before.GetCounter().GetValue() != 1 {
		t.Fatal("truncated response was not recorded as aborted")
	}
}

func TestHTTPStatusLabelsHaveFiniteBudget(t *testing.T) {
	for status := range 700 {
		recordRequest("test", "status-label-budget", "mcp", status, true, time.Millisecond)
	}
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(map[string]bool)
	for _, family := range families {
		if family.GetName() != "mcp_gateway_http_requests_total" {
			continue
		}
		for _, metric := range family.Metric {
			server, status := "", ""
			for _, label := range metric.Label {
				switch label.GetName() {
				case "server":
					server = label.GetValue()
				case "status":
					status = label.GetValue()
				}
			}
			if server == "status-label-budget" {
				statuses[status] = true
			}
		}
	}
	if len(statuses) != 21 || !statuses["200"] || !statuses["2xx"] || !statuses["other"] {
		t.Fatalf("unexpected status-label budget: %v", statuses)
	}
}
