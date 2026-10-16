package recording

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	uploadTestKey  = "recordings/Test/Test-2026-09-28-14-37.mp3"
	uploadTestKey2 = "recordings/Test/Test-2026-09-28-14-37-2.mp3"
	uploadTestKey3 = "recordings/Test/Test-2026-09-28-14-37-3.mp3"
)

// conditionalPutServer fakes S3 PUTs that require If-None-Match: * and, when
// wantBody is set, the whole file on every attempt. respond writes the reply for
// a key and returns false to fall through to 200 OK. It returns the server URL
// and the keys it saw.
func conditionalPutServer(
	t *testing.T,
	wantBody string,
	respond func(w http.ResponseWriter, key string) bool,
) (url string, keys func() []string) {
	t.Helper()

	var (
		mu   sync.Mutex
		seen []string
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
		// Every attempt must send the whole file, not what is left after the last one.
		if body, err := io.ReadAll(req.Body); err != nil || (wantBody != "" && string(body) != wantBody) {
			http.Error(w, fmt.Sprintf("body = %q, %v; want %q", body, err, wantBody), http.StatusBadRequest)
			return
		}
		key, ok := strings.CutPrefix(req.URL.Path, "/bucket/")
		if !ok {
			http.Error(w, "unexpected bucket", http.StatusBadRequest)
			return
		}
		mu.Lock()
		seen = append(seen, key)
		mu.Unlock()

		if !respond(w, key) {
			w.Header().Set("ETag", `"test-etag"`)
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(server.Close)

	return server.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>test</Message></Error>", code)
}

func TestS3UploadUsesSuffixedKeyWhenKeyIsTaken(t *testing.T) {
	t.Parallel()

	taken := func(w http.ResponseWriter, key string) bool {
		switch key {
		case uploadTestKey:
			writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
		case uploadTestKey2:
			writeS3Error(w, http.StatusConflict, "ConditionalRequestConflict")
		default:
			return false
		}
		return true
	}
	allTaken := func(w http.ResponseWriter, _ string) bool {
		writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
		return true
	}
	notImplemented := func(w http.ResponseWriter, _ string) bool {
		writeS3Error(w, http.StatusNotImplemented, "NotImplemented")
		return true
	}

	tests := []struct {
		name     string
		respond  func(http.ResponseWriter, string) bool
		wantErr  bool
		wantKey  string
		wantKeys int
	}{
		{name: "taken and in-progress keys are skipped", respond: taken, wantKey: uploadTestKey3, wantKeys: 3},
		{name: "attempts are bounded", respond: allTaken, wantErr: true, wantKey: withNumericSuffix(uploadTestKey, maxS3KeySuffix), wantKeys: maxS3KeySuffix},
		{name: "other errors are not retried", respond: notImplemented, wantErr: true, wantKey: uploadTestKey, wantKeys: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			url, keys := conditionalPutServer(t, "audio", tt.respond)
			cfg := testS3Recorder()
			cfg.S3Endpoint = url
			recorder := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})

			localPath := filepath.Join(t.TempDir(), filepath.Base(uploadTestKey))
			if err := os.WriteFile(localPath, []byte("audio"), 0o600); err != nil {
				t.Fatal(err)
			}
			req := uploadRequest{localPath: localPath, s3Key: uploadTestKey, fileSize: 5}

			if err := recorder.doUpload(&req); (err != nil) != tt.wantErr {
				t.Fatalf("doUpload() error = %v, want error %t", err, tt.wantErr)
			}
			if req.s3Key != tt.wantKey {
				t.Errorf("S3 key = %q, want %q", req.s3Key, tt.wantKey)
			}
			got := keys()
			if len(got) != tt.wantKeys {
				t.Fatalf("upload keys = %v, want %d attempts", got, tt.wantKeys)
			}
			for i, key := range got[1:] {
				if want := withNumericSuffix(uploadTestKey, i+2); key != want {
					t.Errorf("attempt %d key = %q, want %q", i+2, key, want)
				}
			}
		})
	}
}

// TestS3MultipartUploadUsesSuffixedKeyWhenKeyIsTaken covers hourly files
// (above the 16 MiB multipart threshold): S3 reports the conflict at
// CompleteMultipartUpload, as an error inside an HTTP 200 body.
func TestS3MultipartUploadUsesSuffixedKeyWhenKeyIsTaken(t *testing.T) {
	t.Parallel()

	var (
		mu        sync.Mutex
		completed []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := strings.TrimPrefix(req.URL.Path, "/bucket/")
		query := req.URL.Query()
		_, _ = io.Copy(io.Discard, req.Body)
		switch {
		case req.Method == http.MethodPost && query.Has("uploads"):
			_, _ = io.WriteString(w, "<InitiateMultipartUploadResult><UploadId>u</UploadId></InitiateMultipartUploadResult>")
		case req.Method == http.MethodPut && query.Has("partNumber"):
			w.Header().Set("ETag", `"part"`)
		case req.Method == http.MethodPost && query.Has("uploadId"):
			if req.Header.Get("If-None-Match") != "*" {
				writeS3Error(w, http.StatusBadRequest, "MissingIfNoneMatch")
				return
			}
			mu.Lock()
			completed = append(completed, key)
			mu.Unlock()
			if key == uploadTestKey {
				_, _ = io.WriteString(w, "<Error><Code>PreconditionFailed</Code><Message>taken</Message></Error>")
				return
			}
			_, _ = io.WriteString(w, `<CompleteMultipartUploadResult><ETag>"done"</ETag></CompleteMultipartUploadResult>`)
		case req.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	t.Cleanup(server.Close)

	cfg := testS3Recorder()
	cfg.S3Endpoint = server.URL
	recorder := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
	const size = 17 << 20
	localPath := filepath.Join(t.TempDir(), filepath.Base(uploadTestKey))
	if err := os.WriteFile(localPath, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	req := uploadRequest{localPath: localPath, s3Key: uploadTestKey, fileSize: size}

	if err := recorder.doUpload(&req); err != nil {
		t.Fatalf("doUpload() error = %v", err)
	}
	if req.s3Key != uploadTestKey2 {
		t.Errorf("S3 key = %q, want %q", req.s3Key, uploadTestKey2)
	}
	mu.Lock()
	defer mu.Unlock()
	// The SDK retries the in-body error as a 500 before giving up on the key.
	if want := []string{uploadTestKey, uploadTestKey2}; !slices.Equal(slices.Compact(completed), want) {
		t.Errorf("completed keys = %v, want %v", completed, want)
	}
}

func TestS3ConnectionRejectsStoreWithoutConditionalWrites(t *testing.T) {
	t.Parallel()

	url, _ := conditionalPutServer(t, "", func(w http.ResponseWriter, _ string) bool {
		writeS3Error(w, http.StatusNotImplemented, "NotImplemented")
		return true
	})
	cfg := testS3Recorder()
	cfg.S3Endpoint = url

	err := TestRecorderS3Connection(cfg)
	if err == nil || !strings.Contains(err.Error(), "conditional writes") {
		t.Fatalf("TestRecorderS3Connection() error = %v, want conditional writes error", err)
	}
}
