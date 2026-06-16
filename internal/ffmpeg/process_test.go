package ffmpeg

import (
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
}
