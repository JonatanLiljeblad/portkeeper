package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mcpv1alpha1 "github.com/jonatan/portkeeper/api/v1alpha1"
)

const testPrincipal = "system:serviceaccount:test:agent"

var testAccounts = []mcpv1alpha1.ServiceAccountReference{{Namespace: "test", Name: "agent"}}

type reviewFunc func(context.Context, *authenticationv1.TokenReview, metav1.CreateOptions) (*authenticationv1.TokenReview, error)

func (f reviewFunc) Create(ctx context.Context, review *authenticationv1.TokenReview, opts metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	return f(ctx, review, opts)
}

func validReview() *authenticationv1.TokenReview {
	return &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{"portkeeper"},
		User:          authenticationv1.UserInfo{Username: testPrincipal, UID: "test-uid"},
	}}
}

func testAuthenticator(t *testing.T) *ServiceAccountAuthenticator {
	t.Helper()
	auth, err := NewServiceAccountAuthenticator(reviewFunc(func(ctx context.Context, review *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
		if review.Spec.Token != "test-token" {
			return &authenticationv1.TokenReview{}, nil
		}
		return validReview(), nil
	}), "portkeeper")
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

func TestServiceAccountAuthentication(t *testing.T) {
	for _, tt := range []struct {
		name      string
		headers   []string
		change    func(*authenticationv1.TokenReview)
		apiErr    error
		wantErr   error
		wantCalls int
	}{
		{name: "valid", headers: []string{"Bearer test-token"}, wantCalls: 1},
		{name: "scheme-case-insensitive", headers: []string{"bearer test-token"}, wantCalls: 1},
		{name: "missing", wantErr: ErrUnauthenticated},
		{name: "basic", headers: []string{"Basic test-token"}, wantErr: ErrUnauthenticated},
		{name: "empty-token", headers: []string{"Bearer "}, wantErr: ErrUnauthenticated},
		{name: "extra-token", headers: []string{"Bearer one two"}, wantErr: ErrUnauthenticated},
		{name: "duplicate", headers: []string{"Bearer one", "Bearer two"}, wantErr: ErrUnauthenticated},
		{name: "oversized", headers: []string{"Bearer " + strings.Repeat("x", 16384)}, wantErr: ErrUnauthenticated},
		{name: "invalid-or-expired", headers: []string{"Bearer test-token"}, change: func(r *authenticationv1.TokenReview) { r.Status.Authenticated = false }, wantErr: ErrUnauthenticated, wantCalls: 1},
		{name: "wrong-audience", headers: []string{"Bearer test-token"}, change: func(r *authenticationv1.TokenReview) { r.Status.Audiences = []string{"kubernetes"} }, wantErr: ErrUnauthenticated, wantCalls: 1},
		{name: "missing-audience", headers: []string{"Bearer test-token"}, change: func(r *authenticationv1.TokenReview) { r.Status.Audiences = nil }, wantErr: ErrUnauthenticated, wantCalls: 1},
		{name: "non-serviceaccount", headers: []string{"Bearer test-token"}, change: func(r *authenticationv1.TokenReview) { r.Status.User.Username = "cluster-admin" }, wantErr: ErrUnauthenticated, wantCalls: 1},
		{name: "malformed-serviceaccount", headers: []string{"Bearer test-token"}, change: func(r *authenticationv1.TokenReview) {
			r.Status.User.Username = "system:serviceaccount:test:agent\nforged"
		}, wantErr: ErrUnauthenticated, wantCalls: 1},
		{name: "missing-uid", headers: []string{"Bearer test-token"}, change: func(r *authenticationv1.TokenReview) { r.Status.User.UID = "" }, wantErr: ErrUnauthenticated, wantCalls: 1},
		{name: "review-error", headers: []string{"Bearer test-token"}, change: func(r *authenticationv1.TokenReview) { r.Status.Error = "validation error" }, wantErr: ErrUnauthenticated, wantCalls: 1},
		{name: "api-unavailable", headers: []string{"Bearer test-token"}, apiErr: errors.New("API unavailable"), wantErr: ErrAuthenticationUnavailable, wantCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			auth, err := NewServiceAccountAuthenticator(reviewFunc(func(ctx context.Context, r *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
				calls++
				if r.Spec.Token != "test-token" || !reflect.DeepEqual(r.Spec.Audiences, []string{"portkeeper"}) {
					t.Fatalf("unexpected review spec: audience=%v", r.Spec.Audiences)
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 3*time.Second {
					t.Fatal("review missing bounded deadline")
				}
				review := validReview()
				if tt.change != nil {
					tt.change(review)
				}
				return review, tt.apiErr
			}), "portkeeper")
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/test/demo/mcp", nil)
			req.Header["Authorization"] = tt.headers
			req.Header.Set("X-Agent-ID", "spoofed-admin")
			principal, err := auth.Authenticate(t.Context(), req)
			if !errors.Is(err, tt.wantErr) || calls != tt.wantCalls {
				t.Fatalf("error=%v, calls=%d; want error=%v, calls=%d", err, calls, tt.wantErr, tt.wantCalls)
			}
			if tt.wantErr == nil && principal != testPrincipal {
				t.Fatalf("principal=%q", principal)
			}
			if tt.wantErr != nil && principal != "" {
				t.Fatal("failed authentication returned an identity")
			}
		})
	}
}

func TestAuthenticationUnavailableAtCapacity(t *testing.T) {
	auth := testAuthenticator(t)
	for range cap(auth.inflight) {
		auth.inflight <- struct{}{}
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	if _, err := auth.Authenticate(t.Context(), req); !errors.Is(err, ErrAuthenticationUnavailable) {
		t.Fatalf("capacity error=%v", err)
	}
}

func TestAuthenticationConfiguration(t *testing.T) {
	for _, audience := range []string{"", " portkeeper", "portkeeper "} {
		if _, err := NewServiceAccountAuthenticator(reviewFunc(nil), audience); err == nil {
			t.Fatalf("accepted audience %q", audience)
		}
	}
	if _, err := NewServiceAccountAuthenticator(nil, "portkeeper"); err == nil {
		t.Fatal("accepted missing reviewer")
	}
}

func TestAuthenticationRevalidatesEveryRequest(t *testing.T) {
	calls := 0
	auth, err := NewServiceAccountAuthenticator(reviewFunc(func(ctx context.Context, r *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
		calls++
		result := validReview()
		result.Status.Authenticated = calls == 1
		return result, nil
	}), "portkeeper")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/test/demo/mcp", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Mcp-Session-Id", "existing-session")
	if _, err := auth.Authenticate(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(t.Context(), req); !errors.Is(err, ErrUnauthenticated) || calls != 2 {
		t.Fatalf("revoked token reused: calls=%d, error=%v", calls, err)
	}
}

func TestAuthenticationCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	auth, err := NewServiceAccountAuthenticator(reviewFunc(func(ctx context.Context, r *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}), "portkeeper")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/test/demo/mcp", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	if _, err := auth.Authenticate(ctx, req); !errors.Is(err, ErrAuthenticationUnavailable) {
		t.Fatalf("cancelled authentication returned %v", err)
	}
}
