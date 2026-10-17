package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

func TestPrintPasswordHash(t *testing.T) {
	t.Parallel()
	for _, input := range []string{" passphrase ", " passphrase \n", " passphrase \r\n"} {
		var out bytes.Buffer
		if err := printPasswordHash(strings.NewReader(input), &out); err != nil {
			t.Fatal(err)
		}
		if !util.VerifyPasswordHash(" passphrase ", strings.TrimSuffix(out.String(), "\n")) {
			t.Fatal("CLI changed password bytes")
		}
	}
	for _, input := range []string{"", "\n", "one\ntwo", strings.Repeat("p", 4097)} {
		var out bytes.Buffer
		if err := printPasswordHash(strings.NewReader(input), &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid password produced a hash")
		}
	}
	if err := printPasswordHash(strings.NewReader("secret"), failingHashWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("output error = %v", err)
	}
	if err := printPasswordHash(iotest.ErrReader(io.ErrUnexpectedEOF), io.Discard); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("input error = %v", err)
	}
}

type failingHashWriter struct{}

func (failingHashWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
