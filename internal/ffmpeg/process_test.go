package ffmpeg

import (
	"errors"
	"io"
	"os/exec"
	"testing"
)

func TestStartProcessWithStdoutCapturesOutput(t *testing.T) {
	t.Parallel()
	catPath, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("cat not available")
	}
	result, err := StartProcessWithStdout(catPath, nil)
	if err != nil {
		t.Fatalf("StartProcessWithStdout() error = %v", err)
	}
	if _, err := result.WriteStdin([]byte("encoded bytes")); err != nil {
		t.Fatalf("WriteStdin() error = %v", err)
	}
	result.CloseStdin()
	stdout := result.Stdout()
	if stdout == nil {
		t.Fatal("Stdout() = nil, want reader")
	}
	got, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatalf("ReadAll(stdout) error = %v", err)
	}
	if err := result.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if string(got) != "encoded bytes" {
		t.Fatalf("stdout = %q, want encoded bytes", got)
	}
	if got := result.BytesWritten(); got != int64(len("encoded bytes")) {
		t.Fatalf("BytesWritten() = %d", got)
	}
}

type partialStdin struct{}

func (partialStdin) Write([]byte) (int, error) { return 2, io.ErrClosedPipe }
func (partialStdin) Close() error              { return nil }

func TestBytesWrittenIncludesPartialWrites(t *testing.T) {
	t.Parallel()
	result := &StartResult{stdin: partialStdin{}}
	n, err := result.WriteStdin([]byte("audio"))
	if n != 2 || !errors.Is(err, io.ErrClosedPipe) || result.BytesWritten() != 2 {
		t.Fatalf("partial write = %d, %v; count = %d", n, err, result.BytesWritten())
	}
	result.CloseStdin()
	if _, err := result.WriteStdin([]byte("audio")); !errors.Is(err, ErrStdinClosed) || result.BytesWritten() != 2 {
		t.Fatalf("closed write = %v; count = %d", err, result.BytesWritten())
	}
}
