package ffmpeg

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestCanceledContextPreventsProcessCreation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		start func(context.Context, string, []string) (*StartResult, error)
	}{
		{"stdin", StartProcessContext},
		{"stdout", StartProcessWithStdoutContext},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			result, err := tc.start(ctx, "/nonexistent/ffmpeg-test", nil)
			if result != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled start = %v, %v; want cancellation before exec", result, err)
			}
		})
	}
}

func TestParentCancellationStopsRunningProcess(t *testing.T) {
	t.Parallel()
	path, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("cat not available")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result, err := StartProcessContext(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { result.Cancel(nil); _ = result.Wait() })
	exited := make(chan struct{})
	go func() { _ = result.Wait(); close(exited) }()
	cancel()
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("parent cancellation did not terminate the running process")
	}
	if !errors.Is(context.Cause(result.Context()), context.Canceled) {
		t.Fatal("process did not retain parent cancellation")
	}
}
