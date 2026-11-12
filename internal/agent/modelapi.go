package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// modelEndpoint posts JSON to a model API. It is shared by the planners so
// that key handling, retries and error redaction exist once.
type modelEndpoint struct {
	baseURL    string
	keyFile    string // optional for endpoints that need no key, such as a local model
	client     *http.Client
	retryDelay time.Duration
	// authorize attaches the key and any provider headers.
	authorize func(req *http.Request, key string)
}

// modelAPIError is a non-200 answer from the model API, already redacted.
type modelAPIError struct {
	Status int
	Detail string
}

func (e *modelAPIError) Error() string {
	return fmt.Sprintf("model API returned HTTP %d (%s)", e.Status, e.Detail)
}

// post sends one request, retrying only overload, server and transport
// errors. A model call has no side effects, unlike a tool call, so retrying
// it is safe.
func (e modelEndpoint) post(ctx context.Context, path string, body []byte, out any) error {
	client := e.client
	if client == nil {
		client = &http.Client{Timeout: modelCallTimeout}
	}
	delay := e.retryDelay
	if delay <= 0 {
		delay = 2 * time.Second
	}
	var last error
	for attempt := 0; attempt <= maxModelRetries; attempt++ {
		if attempt != 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
		}
		retry, err := e.attempt(ctx, client, path, body, out)
		if err == nil {
			return nil
		}
		last = err
		if !retry {
			break
		}
	}
	return last
}

func (e modelEndpoint) attempt(ctx context.Context, client *http.Client, path string, body []byte, out any) (retry bool, err error) {
	secret := ""
	if e.keyFile != "" {
		key, err := os.ReadFile(e.keyFile)
		if err != nil {
			return false, errors.New("cannot read the model API key file")
		}
		secret = strings.TrimSpace(string(key))
		if secret == "" || strings.ContainsAny(secret, " \t\r\n") {
			return false, errors.New("model API key file must contain exactly one key")
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(e.baseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return false, errors.New("invalid model API base URL")
	}
	req.Header.Set("Content-Type", "application/json")
	if e.authorize != nil {
		e.authorize(req, secret)
	}
	res, err := client.Do(req)
	if err != nil {
		// Transport errors can embed the URL but never request headers.
		return ctx.Err() == nil, fmt.Errorf("model API request failed: %w", err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return true, fmt.Errorf("read model API response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		detail := errorDetail(data)
		if secret != "" {
			detail = strings.ReplaceAll(detail, secret, "[redacted]")
		}
		if len(detail) > maxErrorTextBytes {
			detail = detail[:maxErrorTextBytes]
		}
		return res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500,
			&modelAPIError{Status: res.StatusCode, Detail: detail}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return false, errors.New("model API returned an unreadable response")
	}
	return false, nil
}

// errorDetail reads the provider's message. Providers use either
// {"error":{"type":..,"message":..}} or {"error":"text"}.
func errorDetail(data []byte) string {
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &body) != nil || len(body.Error) == 0 {
		return "no error detail"
	}
	var text string
	if json.Unmarshal(body.Error, &text) == nil {
		return text
	}
	var object struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body.Error, &object)
	return strings.Trim(object.Type+": "+object.Message, ": ")
}
