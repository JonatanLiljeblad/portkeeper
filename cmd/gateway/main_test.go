package main

import (
	"os"
	"os/exec"
	"testing"
)

func TestRateLimitEnvironment(t *testing.T) {
	if os.Getenv("PORTKEEPER_ENV_TEST_CHILD") == "1" {
		envFloat("GATEWAY_RATE_LIMIT_RPS", 5)
		envInt("GATEWAY_RATE_LIMIT_BURST", 10)
		envFloat32("GATEWAY_TOKEN_REVIEW_QPS", 5)
		return
	}
	for _, tt := range []struct {
		name, rps, burst, reviewQPS string
		valid                       bool
	}{
		{"defaults", "", "", "", true},
		{"positive", "0.5", "1", "", true},
		{"nan", "NaN", "10", "", false},
		{"infinity", "+Inf", "10", "", false},
		{"zero-rate", "0", "10", "", false},
		{"negative-rate", "-1", "10", "", false},
		{"invalid-rate", "abc", "10", "", false},
		{"zero-burst", "5", "0", "", false},
		{"fractional-burst", "5", "1.5", "", false},
		{"review-overflow", "", "", "1e99", false},
		{"review-underflow", "", "", "1e-99", false},
		{"review-zero", "", "", "0", false},
		{"benchmark-profile", "1000", "1000", "1000", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PORTKEEPER_ENV_TEST_CHILD", "1")
			t.Setenv("GATEWAY_RATE_LIMIT_RPS", tt.rps)
			t.Setenv("GATEWAY_RATE_LIMIT_BURST", tt.burst)
			t.Setenv("GATEWAY_TOKEN_REVIEW_QPS", tt.reviewQPS)
			cmd := exec.Command(os.Args[0], "-test.run=^TestRateLimitEnvironment$")
			output, err := cmd.CombinedOutput()
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v, error=%v, output=%s", tt.valid, err, output)
			}
		})
	}
}
