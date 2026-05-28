package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestDoWithRetryRespectsContextCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		attempts := 0
		client := &GraphClient{
			fromAddress: "sender@example.com",
			httpClient: &http.Client{
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

		err := <-done
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("doWithRetry() error = %v, want context.Canceled", err)
		}
	})
}
