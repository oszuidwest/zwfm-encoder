package recording

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
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
		if req.Method == http.MethodHead {
			w.Header().Set(testUploadIDHeader, "another-recording")
			w.Header().Set("Content-Length", "0")
			return
		}
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

			pending := pendingUpload{request: req}
			if err := recorder.doUpload(t.Context(), &pending); (err != nil) != tt.wantErr {
				t.Fatalf("doUpload() error = %v, want error %t", err, tt.wantErr)
			}
			req = pending.request
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

	store, cfg := newIdentityStore(t)
	store.inBodyConflict = true
	store.objects[uploadTestKey] = storedTestUpload{id: "foreign", body: []byte("earlier")}
	recorder := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
	const size = 17 << 20
	localPath := filepath.Join(t.TempDir(), filepath.Base(uploadTestKey))
	if err := os.WriteFile(localPath, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	req := uploadRequest{localPath: localPath, s3Key: uploadTestKey, fileSize: size}

	pending := pendingUpload{request: req}
	if err := recorder.doUpload(t.Context(), &pending); err != nil {
		t.Fatalf("doUpload() error = %v", err)
	}
	req = pending.request
	if req.s3Key != uploadTestKey2 {
		t.Errorf("S3 key = %q, want %q", req.s3Key, uploadTestKey2)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	// The SDK retries the in-body error as a 500 before giving up on the key.
	if want := []string{uploadTestKey, uploadTestKey2}; !slices.Equal(slices.Compact(store.keys), want) {
		t.Errorf("completed keys = %v, want %v", store.keys, want)
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

// newHangingStore returns an identityStore whose PUTs stall until heal is
// called, like a blackholed S3 endpoint; a PUT whose client gives up fails.
// entered receives once per stalled PUT, up to its buffer. Cleanup heals the
// store.
func newHangingStore(t *testing.T) (store *identityStore, cfg *types.Recorder, heal func(), entered <-chan struct{}) {
	t.Helper()
	store = &identityStore{objects: make(map[string]storedTestUpload), multipart: make(map[string]*multipartTestUpload)}
	healed := make(chan struct{})
	heal = sync.OnceFunc(func() { close(healed) })
	stalled := make(chan struct{}, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut {
			select {
			case <-healed:
			default:
				select {
				case stalled <- struct{}{}:
				default: // Nobody counts this many stalls.
				}
				select {
				case <-req.Context().Done():
					return
				case <-healed:
				}
			}
		}
		store.serveHTTP(w, req)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(heal) // Before Close, which waits for stalled handlers.
	cfg = testS3Recorder()
	cfg.S3Endpoint = server.URL
	return store, cfg, heal, stalled
}

// testEventLogger returns a recorder event logger and a reader for the
// event types it logged.
func testEventLogger(t *testing.T) (logger *eventlog.Logger, logged func() []eventlog.EventType) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	logger, err := eventlog.NewLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	return logger, func() []eventlog.EventType {
		events, _, err := eventlog.ReadLast(path, 100, 0, eventlog.FilterRecorder)
		if err != nil {
			t.Fatal(err)
		}
		eventTypes := make([]eventlog.EventType, 0, len(events))
		for _, event := range events {
			eventTypes = append(eventTypes, event.Type)
		}
		return eventTypes
	}
}

func TestStopDefersHangingUploadsToRetryQueue(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		mode     types.StorageMode
		uploaded string // The upload the stop interrupts.
	}{
		{name: "s3", mode: types.StorageS3, uploaded: "previous-hour"},
		{name: "both", mode: types.StorageBoth, uploaded: "previous-hour"},
		// Over the multipart threshold: the stop must abort the multipart upload.
		{name: "multipart", mode: types.StorageS3, uploaded: strings.Repeat("a", 17<<20)},
	} {
		mode := tc.mode
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, cfg, heal, entered := newHangingStore(t)
			cfg.StorageMode = mode
			if mode == types.StorageBoth {
				cfg.LocalPath = t.TempDir()
			}
			spoolDir := t.TempDir()
			recording := func(name, data string) string {
				if mode == types.StorageS3 {
					return writeSpoolFile(t, spoolDir, cfg.ID, name, data)
				}
				path := filepath.Join(cfg.LocalPath, name)
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			}
			logger, loggedEvents := testEventLogger(t)
			r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: spoolDir, EventLogger: logger})
			r.mu.Lock()
			r.state = types.ProcessRunning
			r.startUploadWorkerLocked()
			r.mu.Unlock()

			// The previous hour is uploading; the final file of this run is queued behind it.
			uploading := recording("Test-2026-10-21-10-00.mp3", tc.uploaded)
			r.queueForUpload(uploading)
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("upload did not start")
			}
			queued := recording("Test-2026-10-21-11-00.mp3", "final-file")
			r.queueForUpload(queued)

			// Hold off the retry pass Stop starts, so what the stop left is
			// seen before any attempt.
			r.retryProcessMu.Lock()
			within(t, "Stop", r.Stop)
			store.mu.Lock()
			incomplete := len(store.multipart)
			store.mu.Unlock()
			if incomplete != 0 {
				t.Errorf("stop left %d incomplete multipart uploads on S3", incomplete)
			}
			// Neither upload failed: both are persisted for retry, so a
			// restarted process finds them too, without a counted attempt.
			pending, err := r.loadPendingUploads()
			if err != nil || len(pending) != 2 {
				t.Fatalf("persisted uploads = %d, %v; want both files", len(pending), err)
			}
			for _, p := range pending {
				if p.retryCount != 0 || p.lastError != uploadStoppedReason {
					t.Errorf("deferred upload %s: retries %d, last error %q", filepath.Base(p.request.localPath), p.retryCount, p.lastError)
				}
			}
			if got := r.PendingUploadCount(); got != 0 {
				t.Errorf("pending uploads = %d, want 0: a deferred upload has not failed", got)
			}
			r.retryProcessMu.Unlock()

			// The held-off pass uploads both once S3 answers.
			heal()
			r.processRetryQueue()
			waitForUploadCleanup(t, store, r, uploading)
			waitForUploadCleanup(t, store, r, queued)
			if logged := loggedEvents(); slices.Contains(logged, eventlog.UploadFailed) {
				t.Errorf("stop logged an upload failure: %v", logged)
			}
		})
	}
}

// TestUploadWorkerDefersBacklogOnceStopBegins covers a file queued behind the
// current upload while a stop is still finalizing: the worker must not start
// an upload that the stop is about to interrupt.
func TestUploadWorkerDefersBacklogOnceStopBegins(t *testing.T) {
	t.Parallel()
	_, cfg, _, entered := newHangingStore(t)
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
	r.mu.Lock()
	r.startUploadWorkerLocked()
	r.uploadClosed = true // As Stop does before finalizing.
	cancel := r.cancelUploads
	r.mu.Unlock()
	t.Cleanup(func() { cancel(); r.uploadWg.Wait() }) // Before the spool is removed.
	file := writeSpoolFile(t, r.spoolDir, r.id, filepath.Base(uploadTestKey), "audio")
	r.uploadQueue <- uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: 5, deleteAfterUpload: true}
	deferred := func() bool {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return len(r.retryQueue) == 1 && r.retryQueue[0].lastError == uploadStoppedReason
	}
	for deadline := time.Now().Add(3 * time.Second); !deferred() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	select {
	case <-entered:
		t.Fatal("the worker started an upload after the stop began")
	default:
	}
	if !deferred() {
		t.Fatal("the backlogged upload was not left to the retry queue")
	}
}

// TestDeferredUploadThatFailsCountsAsFailed covers a deferred upload whose
// first attempt fails: from then on it is reported, counted and abandoned
// like any failed upload.
func TestDeferredUploadThatFailsCountsAsFailed(t *testing.T) {
	t.Parallel()
	var fixed atomic.Bool
	var attempts, uploads atomic.Int32
	server := httptest.NewServer(abandonedUploadHandler(t, &fixed, &attempts, &uploads))
	t.Cleanup(server.Close)
	cfg := testS3Recorder()
	cfg.S3Endpoint = server.URL
	eventPath := filepath.Join(t.TempDir(), "events.jsonl")
	logger, err := eventlog.NewLogger(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	var alerts []UploadAbandonedEvent
	r := NewGenericRecorder(GenericRecorderConfig{
		Recorder: cfg, SpoolDir: t.TempDir(), EventLogger: logger,
		OnUploadAbandoned: func(event UploadAbandonedEvent) { alerts = append(alerts, event) },
	})
	file := writeSpoolFile(t, r.spoolDir, r.id, filepath.Base(uploadTestKey), "audio")
	r.addToRetryQueue(uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: 5, deleteAfterUpload: true}, uploadStoppedReason)
	if got := r.PendingUploadCount(); got != 0 {
		t.Fatalf("pending uploads = %d, want 0: a deferred upload has not failed", got)
	}

	seen := 0
	pass := func(want ...eventlog.EventType) []eventlog.Event {
		t.Helper()
		r.processRetryQueue()
		events, _, err := eventlog.ReadLast(eventPath, 100, 0, eventlog.FilterRecorder)
		if err != nil {
			t.Fatal(err)
		}
		slices.Reverse(events) // Oldest first.
		logged := events[seen:]
		seen = len(events)
		var got []eventlog.EventType
		for _, event := range logged {
			got = append(got, event.Type)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("retry pass logged %v, want %v", got, want)
		}
		return logged
	}

	// Like a worker upload, the first attempt is retry 0 and the next is 1.
	wantRetries := func(logged []eventlog.Event, want ...any) {
		t.Helper()
		for i, event := range logged {
			if details, _ := event.Details.(map[string]any); details["retry"] != want[i] {
				t.Errorf("%s details = %v, want retry %v", event.Type, details, want[i])
			}
		}
	}
	wantRetries(pass(eventlog.UploadQueued, eventlog.UploadFailed), nil, nil)
	if r.retryQueue[0].lastError == uploadStoppedReason || r.PendingUploadCount() != 1 {
		t.Fatalf("after a failed attempt: last error %q, pending %d; want the S3 error, 1", r.retryQueue[0].lastError, r.PendingUploadCount())
	}
	wantRetries(pass(eventlog.UploadRetry, eventlog.UploadFailed), 1.0, 1.0)
	r.retryQueue[0].firstAttempt = time.Now().Add(-MaxUploadRetryAge - time.Hour)
	wantRetries(pass(eventlog.UploadAbandoned), 1.0)
	if len(alerts) != 1 || alerts[0].LastError == uploadStoppedReason || alerts[0].RetryCount != 1 || attempts.Load() != 2 {
		t.Fatalf("abandonment alerts %+v after %d attempts; want one with the S3 error and retry count 1 after 2", alerts, attempts.Load())
	}
}

// recordTestAudio feeds the running recorder audio, so its file is uploaded
// instead of discarded as empty, and returns that file.
func recordTestAudio(t *testing.T, r *GenericRecorder) string {
	t.Helper()
	r.mu.RLock()
	result, file := r.result, r.currentFile
	r.mu.RUnlock()
	r.WriteAudio(make([]byte, 4096))
	for deadline := time.Now().Add(3 * time.Second); result.BytesWritten() == 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if result.BytesWritten() == 0 {
		t.Fatal("encoder did not accept test audio")
	}
	return file
}

// waitForUploadCleanup waits until file is stored and its upload metadata
// and, in S3-only mode, its spool copy are gone: the last filesystem changes
// a retry pass makes for it.
func waitForUploadCleanup(t *testing.T, store *identityStore, r *GenericRecorder, file string) {
	t.Helper()
	cfg := r.Config()
	key := s3ObjectKey(&cfg, filepath.Base(file))
	done := func() bool {
		store.mu.Lock()
		_, uploaded := store.objects[key]
		store.mu.Unlock()
		_, err := os.Stat(file)
		entries, _ := os.ReadDir(r.pendingUploadDir())
		return uploaded && os.IsNotExist(err) == (cfg.StorageMode == types.StorageS3) && len(entries) == 0
	}
	for deadline := time.Now().Add(5 * time.Second); !done() && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if !done() {
		t.Fatalf("%s was not uploaded and cleaned up", filepath.Base(file))
	}
}

// waitForRetryPass waits until a retry pass, such as the one Stop starts, has
// uploaded every file of r: none is queued, in flight or persisted for retry.
func waitForRetryPass(t *testing.T, r *GenericRecorder) {
	t.Helper()
	left := func() int {
		r.mu.RLock()
		n := len(r.retryQueue) + len(r.retryInFlight)
		r.mu.RUnlock()
		entries, _ := os.ReadDir(r.pendingUploadDir())
		return max(n, len(entries))
	}
	for deadline := time.Now().Add(5 * time.Second); left() != 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if n := left(); n != 0 {
		t.Fatalf("%d uploads still waiting for a retry pass", n)
	}
}

// within fails the test when f does not return within three seconds.
func within(t *testing.T, name string, f func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("%s waited for a hanging S3 upload", name)
	}
}

// TestStopRetriesFinalFile covers stops that no Start may follow: the stop
// retries its final file, which S3 cannot accept before the stop returns,
// by itself instead of at the next hourly retry.
func TestStopRetriesFinalFile(t *testing.T) {
	var (
		mu     sync.Mutex
		limits []func()
	)
	afterFunc = func(_ time.Duration, limit func()) *time.Timer {
		mu.Lock()
		defer mu.Unlock()
		limits = append(limits, limit)
		return time.NewTimer(math.MaxInt64)
	}
	t.Cleanup(func() { afterFunc = time.AfterFunc })
	// Each stop releases FFmpeg finalization, which waits for it.
	type stopFunc func(m *Manager, r *GenericRecorder, finalize func()) error
	finalizeThen := func(stop func(m *Manager, id string) error) stopFunc {
		return func(m *Manager, r *GenericRecorder, finalize func()) error {
			finalize()
			return stop(m, r.id)
		}
	}
	for _, tc := range []struct {
		name string
		stop stopFunc
	}{
		// An explicit encoder stop.
		{name: "manager stop", stop: finalizeThen(func(m *Manager, _ string) error { return m.Stop() })},
		{name: "stop", stop: finalizeThen((*Manager).StopRecorder)},
		{name: "remove", stop: finalizeThen((*Manager).RemoveRecorder)},
		// The limit stops the recorder through stopLocked, not Stop.
		{name: "max duration", stop: finalizeThen(func(*Manager, string) error {
			mu.Lock()
			limit := limits[len(limits)-1]
			mu.Unlock()
			limit()
			return nil
		})},
		// The removal returns while the manager's stop has not queued the
		// final file yet, so that stop must start the pass.
		{name: "remove during manager stop", stop: func(m *Manager, r *GenericRecorder, finalize func()) error {
			stopped := make(chan error, 1)
			go func() { stopped <- m.Stop() }()
			for r.Status().State != types.ProcessStopping {
				time.Sleep(time.Millisecond)
			}
			if err := m.RemoveRecorder(r.id); err != nil {
				return err
			}
			finalize()
			return <-stopped
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			barrier := filepath.Join(t.TempDir(), "finalize")
			t.Setenv(fakeFFmpegFinalizeBarrierEnv, barrier)
			finalize := func() {
				if err := os.WriteFile(barrier, nil, 0o600); err != nil {
					t.Error(err)
				}
			}
			store, cfg, heal, _ := newHangingStore(t)
			cfg.RecordingMode = types.RecordingOnDemand
			// os.Executable, not os.Args: gosec taints the args into the spool paths.
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			logger, loggedEvents := testEventLogger(t)
			m, err := NewManager(executable, t.TempDir(), 60, logger)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.AddRecorder(cfg); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { heal(); _ = m.Stop() })
			t.Cleanup(finalize) // Never leave a fake FFmpeg waiting.
			if err := m.Start(); err != nil {
				t.Fatal(err)
			}
			r := m.recorders[cfg.ID]
			if err := m.StartRecorder(cfg.ID); err != nil {
				t.Fatal(err)
			}
			waitForRecorderState(t, r, types.ProcessRunning)
			file := recordTestAudio(t, r)
			within(t, tc.name, func() error { return tc.stop(m, r, finalize) })
			waitForRecorderState(t, r, types.ProcessStopped)
			store.mu.Lock()
			early := len(store.objects)
			store.mu.Unlock()
			if early != 0 {
				t.Fatal("final file was uploaded before the store accepted uploads")
			}
			heal()
			waitForUploadCleanup(t, store, r, file)
			// The stop sends the final file straight to retry storage instead of
			// starting an upload it then interrupts, and its deferred upload is
			// queued once, not reported as a failed upload's retry.
			logged := loggedEvents()
			queued := 0
			for _, e := range logged {
				if e == eventlog.UploadQueued {
					queued++
				}
			}
			if queued != 1 || slices.Contains(logged, eventlog.UploadRetry) {
				t.Errorf("want one upload_queued and no upload_retry for the final file: %v", logged)
			}
		})
	}
}

// TestRemoveStoppedRecorderRetriesItsUploads covers a recorder whose stop is
// long over: no later manager pass covers it once removed, so the removal
// retries what it left queued.
func TestRemoveStoppedRecorderRetriesItsUploads(t *testing.T) {
	t.Parallel()
	store, cfg := newIdentityStore(t)
	cfg.RecordingMode = types.RecordingOnDemand
	spoolDir := t.TempDir()
	m := restartManager(t, spoolDir, cfg)
	r := m.recorders[cfg.ID]
	file := writeSpoolFile(t, spoolDir, cfg.ID, "final.mp3", "audio")
	rc := r.Config()
	r.addToRetryQueue(uploadRequest{localPath: file, s3Key: s3ObjectKey(&rc, "final.mp3"), fileSize: 5, deleteAfterUpload: true}, "s3 unavailable")
	if err := m.RemoveRecorder(cfg.ID); err != nil {
		t.Fatal(err)
	}
	waitForUploadCleanup(t, store, r, file)
}
