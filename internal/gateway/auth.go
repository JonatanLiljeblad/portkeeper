package gateway

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

var (
	ErrUnauthenticated           = errors.New("a valid gateway-audience ServiceAccount bearer token is required")
	ErrAuthenticationUnavailable = errors.New("authentication service unavailable")
)

type Authenticator interface {
	Authenticate(context.Context, *http.Request) (string, error)
}

type tokenReviewer interface {
	Create(context.Context, *authenticationv1.TokenReview, metav1.CreateOptions) (*authenticationv1.TokenReview, error)
}

// ServiceAccountAuthenticator delegates signature, expiry and bound-object
// validation to Kubernetes. Reviews are not cached, including across requests
// in an MCP session.
type ServiceAccountAuthenticator struct {
	reviewer tokenReviewer
	audience string
	inflight chan struct{}
}

func NewServiceAccountAuthenticator(reviewer tokenReviewer, audience string) (*ServiceAccountAuthenticator, error) {
	if reviewer == nil || audience == "" || strings.TrimSpace(audience) != audience {
		return nil, errors.New("token reviewer and a nonempty, unpadded audience are required")
	}
	return &ServiceAccountAuthenticator{
		reviewer: reviewer, audience: audience, inflight: make(chan struct{}, 32),
	}, nil
}

func (a *ServiceAccountAuthenticator) Authenticate(ctx context.Context, req *http.Request) (string, error) {
	headers := req.Header.Values("Authorization")
	if len(headers) != 1 || len(headers[0]) > 16384 {
		return "", ErrUnauthenticated
	}
	parts := strings.Fields(headers[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", ErrUnauthenticated
	}
	select {
	case a.inflight <- struct{}{}:
		defer func() { <-a.inflight }()
	default:
		return "", ErrAuthenticationUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	review, err := a.reviewer.Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: parts[1], Audiences: []string{a.audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		// Do not log API response bodies or errors that could contain a token.
		log.Printf("authentication_unavailable reason=%s", apierrors.ReasonForError(err))
		return "", ErrAuthenticationUnavailable
	}
	if review == nil || !review.Status.Authenticated || review.Status.Error != "" ||
		!slices.Contains(review.Status.Audiences, a.audience) || review.Status.User.UID == "" ||
		!validServiceAccount(review.Status.User.Username) {
		return "", ErrUnauthenticated
	}
	return review.Status.User.Username, nil
}

func validServiceAccount(username string) bool {
	parts := strings.Split(username, ":")
	return len(parts) == 4 && parts[0] == "system" && parts[1] == "serviceaccount" &&
		len(validation.IsDNS1123Label(parts[2])) == 0 &&
		len(validation.IsDNS1123Subdomain(parts[3])) == 0
}
