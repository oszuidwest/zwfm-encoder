package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func validGraphClientConstructorConfig() *types.GraphConfig {
	return &types.GraphConfig{
		TenantID:     "tenant.onmicrosoft.com",
		ClientID:     "client-id",
		ClientSecret: "secret",
		FromAddress:  "sender@example.com",
	}
}

func newStaticGraphHTTPClient(statusCode int, body string) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: statusCode,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		}),
	}
}

func TestNewGraphClientWithHTTPClientBuildsClient(t *testing.T) {
	t.Parallel()

	client, err := newGraphClientWithHTTPClient(
		validGraphClientConstructorConfig(),
		newStaticGraphHTTPClient(http.StatusNoContent, ""),
	)
	if err != nil {
		t.Fatalf("newGraphClientWithHTTPClient() error = %v, want nil", err)
	}
	if client.fromAddress != "sender@example.com" {
		t.Fatalf("fromAddress = %q, want sender@example.com", client.fromAddress)
	}

	_, err = newGraphClientWithHTTPClient(validGraphClientConstructorConfig(), nil)
	if err == nil || err.Error() != "http client is required" {
		t.Fatalf("nil http client error = %v, want http client required error", err)
	}
}

func TestNewGraphClientPreservesClientValidation(t *testing.T) {
	t.Parallel()

	_, err := NewGraphClient(&types.GraphConfig{
		TenantID:     "tenant.onmicrosoft.com",
		ClientID:     "client-id",
		ClientSecret: "secret",
	})
	if err == nil || err.Error() != "from address (shared mailbox) is required" {
		t.Fatalf("missing from_address error = %v, want legacy from_address error", err)
	}
}

func TestGraphClientSendMailUsesInjectedHTTPClient(t *testing.T) {
	t.Parallel()

	attempts := 0
	client, err := newGraphClientWithHTTPClient(validGraphClientConstructorConfig(), &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempts++
			if req.Method != http.MethodPost {
				t.Fatalf("request method = %s, want POST", req.Method)
			}
			if req.URL.String() != "https://graph.microsoft.com/v1.0/users/sender@example.com/sendMail" {
				t.Fatalf("request URL = %s, want Graph sendMail URL", req.URL.String())
			}
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("bad request")),
				Request:    req,
			}, nil
		}),
	})
	if err != nil {
		t.Fatalf("newGraphClientWithHTTPClient() error = %v, want nil", err)
	}

	err = client.SendMail(context.Background(), []string{"recipient@example.com"}, "subject", "body")
	if err == nil || err.Error() != "graph API error 400: bad request" {
		t.Fatalf("SendMail() error = %v, want Graph 400 error", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestGraphClientValidateAuthUsesInjectedHTTPClient(t *testing.T) {
	t.Parallel()

	client, err := newGraphClientWithHTTPClient(
		validGraphClientConstructorConfig(),
		newStaticGraphHTTPClient(http.StatusNotFound, "mailbox missing"),
	)
	if err != nil {
		t.Fatalf("newGraphClientWithHTTPClient() error = %v, want nil", err)
	}

	err = client.ValidateAuth()
	if err == nil || err.Error() != "mailbox sender@example.com not found" {
		t.Fatalf("ValidateAuth() error = %v, want mailbox not found error", err)
	}
}

func TestDoWithRetryRespectsContextCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		attempts := 0
		client, err := newGraphClientWithHTTPClient(
			validGraphClientConstructorConfig(),
			&http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					attempts++
					return &http.Response{
						StatusCode: http.StatusBadGateway,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader("temporary failure")),
						Request:    req,
					}, nil
				}),
			},
		)
		if err != nil {
			t.Fatalf("newGraphClientWithHTTPClient() error = %v, want nil", err)
		}

		done := make(chan error, 1)
		go func() {
			done <- client.doWithRetry(ctx, []byte(`{"message":{}}`))
		}()
		synctest.Wait()

		if attempts != 1 {
			t.Fatalf("attempts before cancellation = %d, want 1", attempts)
		}

		cancel()
		synctest.Wait()

		err = <-done
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("doWithRetry() error = %v, want context.Canceled", err)
		}
	})
}
