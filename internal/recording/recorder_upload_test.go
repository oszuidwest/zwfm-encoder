package recording

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestS3UploadDoesNotOverwriteObjectAfterProcessRestart(t *testing.T) {
	t.Parallel()

	const (
		originalKey = "recordings/Test/Test-2026-09-28-14-37.mp3"
		suffixedKey = "recordings/Test/Test-2026-09-28-14-37-2.mp3"
	)

	var (
		mu       sync.Mutex
		requests []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPut {
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		if got := req.Header.Get("If-None-Match"); got != "*" {
			http.Error(w, fmt.Sprintf("If-None-Match = %q, want *", got), http.StatusBadRequest)
			return
		}
		if _, err := io.Copy(io.Discard, req.Body); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		key := req.URL.Path[len("/bucket/"):]
		mu.Lock()
		requests = append(requests, key)
		mu.Unlock()

		switch key {
		case originalKey:
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(w, "<Error><Code>PreconditionFailed</Code><Message>object exists</Message></Error>")
		case suffixedKey:
			w.Header().Set("ETag", `"test-etag"`)
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected key", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	cfg := testS3Recorder()
	cfg.S3Endpoint = server.URL
	recorder := NewGenericRecorder(GenericRecorderConfig{
		Recorder: cfg,
		SpoolDir: t.TempDir(),
	})

	localPath := filepath.Join(t.TempDir(), filepath.Base(originalKey))
	if err := os.WriteFile(localPath, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := uploadRequest{
		localPath: localPath,
		s3Key:     originalKey,
		fileSize:  5,
	}

	if err := recorder.doUpload(&req); err != nil {
		t.Fatalf("doUpload() error = %v", err)
	}
	if req.s3Key != suffixedKey {
		t.Fatalf("S3 key = %q, want %q", req.s3Key, suffixedKey)
	}

	mu.Lock()
	defer mu.Unlock()
	wantRequests := []string{originalKey, suffixedKey}
	if len(requests) != len(wantRequests) {
		t.Fatalf("upload keys = %v, want %v", requests, wantRequests)
	}
	for i := range wantRequests {
		if requests[i] != wantRequests[i] {
			t.Fatalf("upload key %d = %q, want %q", i, requests[i], wantRequests[i])
		}
	}
}
