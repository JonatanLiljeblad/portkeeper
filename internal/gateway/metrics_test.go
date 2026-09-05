package gateway

import (
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
)

func TestMetricsNamespaceLabels(t *testing.T) {
	for _, namespace := range []string{"metrics-one", "metrics-two"} {
		counter := toolCallsTotal.WithLabelValues(namespace, "same", "mcp", "200")
		before := &dto.Metric{}
		if err := counter.Write(before); err != nil {
			t.Fatal(err)
		}
		recordCall(namespace, "same", "mcp", 200, time.Millisecond)
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
