package recording

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

const testUploadIDHeader = "X-Amz-Meta-Zwfm-Upload-Id"

type storedTestUpload struct {
	id   string
	body []byte
}

type multipartTestUpload struct {
	id    string
	parts map[int][]byte
}

// identityStore models committed objects separately from in-progress multipart uploads.
type identityStore struct {
	mu              sync.Mutex
	objects         map[string]storedTestUpload
	multipart       map[string]*multipartTestUpload
	nextID          int
	loseResponse    bool
	headStatus      int
	headRequests    int
	rejectKey       string
	stripIdentity   bool
	foreignIdentity bool
	inBodyConflict  bool
	beforeWrite     func(string)
	keys            []string
}

func newIdentityStore(t *testing.T) (*identityStore, *types.Recorder) {
	t.Helper()
	store := &identityStore{objects: make(map[string]storedTestUpload), multipart: make(map[string]*multipartTestUpload)}
	server := httptest.NewServer(http.HandlerFunc(store.serveHTTP))
	t.Cleanup(server.Close)
	cfg := testS3Recorder()
	cfg.S3Endpoint = server.URL
	return store, cfg
}

func (s *identityStore) serveHTTP(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := strings.TrimPrefix(req.URL.Path, "/bucket/")
	query := req.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case req.Method == http.MethodHead:
		s.headRequests++
		if s.headStatus != 0 {
			w.WriteHeader(s.headStatus)
			return
		}
		object, exists := s.objects[key]
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set(testUploadIDHeader, object.id)
		w.Header().Set("Content-Length", strconv.Itoa(len(object.body)))
		w.Header().Set("ETag", `"object"`)
	case req.Method == http.MethodPost && query.Has("uploads"):
		if s.beforeWrite != nil {
			s.beforeWrite(req.Header.Get(testUploadIDHeader))
		}
		s.nextID++
		id := strconv.Itoa(s.nextID)
		s.multipart[id] = &multipartTestUpload{id: req.Header.Get(testUploadIDHeader), parts: make(map[int][]byte)}
		_, _ = fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case req.Method == http.MethodPut && query.Has("partNumber"):
		upload := s.multipart[query.Get("uploadId")]
		if upload == nil {
			writeS3Error(w, http.StatusNotFound, "NoSuchUpload")
			return
		}
		part, _ := strconv.Atoi(query.Get("partNumber"))
		upload.parts[part] = body
		w.Header().Set("ETag", `"part"`)
	case req.Method == http.MethodPost && query.Has("uploadId"):
		s.completeMultipart(w, req, key)
	case req.Method == http.MethodPut:
		if s.beforeWrite != nil {
			s.beforeWrite(req.Header.Get(testUploadIDHeader))
		}
		if s.commit(w, req, key, storedTestUpload{id: req.Header.Get(testUploadIDHeader), body: body}) {
			w.Header().Set("ETag", `"object"`)
		}
	case req.Method == http.MethodDelete:
		if query.Has("uploadId") {
			delete(s.multipart, query.Get("uploadId"))
		} else {
			delete(s.objects, key)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected operation", http.StatusBadRequest)
	}
}

func (s *identityStore) completeMultipart(w http.ResponseWriter, req *http.Request, key string) {
	id := req.URL.Query().Get("uploadId")
	upload := s.multipart[id]
	if upload == nil {
		writeS3Error(w, http.StatusNotFound, "NoSuchUpload")
		return
	}
	var parts [][]byte
	for _, part := range slices.Sorted(maps.Keys(upload.parts)) {
		parts = append(parts, upload.parts[part])
	}
	if s.commit(w, req, key, storedTestUpload{id: upload.id, body: bytes.Join(parts, nil)}) {
		delete(s.multipart, id)
		_, _ = io.WriteString(w, `<CompleteMultipartUploadResult><ETag>"object"</ETag></CompleteMultipartUploadResult>`)
	} else if object, committed := s.objects[key]; committed && object.id == upload.id {
		// A committed upload ID no longer exists, even when its response was lost.
		delete(s.multipart, id)
	}
}

// commit returns false when a response has already been written, including lost success.
func (s *identityStore) commit(w http.ResponseWriter, req *http.Request, key string, object storedTestUpload) bool {
	s.keys = append(s.keys, key)
	if req.Header.Get("If-None-Match") != "*" {
		writeS3Error(w, http.StatusBadRequest, "MissingIfNoneMatch")
		return false
	}
	if _, exists := s.objects[key]; exists {
		status := http.StatusPreconditionFailed
		if s.inBodyConflict && req.Method == http.MethodPost {
			status = http.StatusOK
		}
		writeS3Error(w, status, "PreconditionFailed")
		return false
	}
	if key == s.rejectKey {
		writeS3Error(w, http.StatusForbidden, "AccessDenied")
		return false
	}
	if s.stripIdentity {
		object.id = ""
	}
	if s.foreignIdentity {
		object.id = "foreign"
	}
	s.objects[key] = object
	if s.loseResponse {
		s.loseResponse = false
		writeS3Error(w, http.StatusServiceUnavailable, "SlowDown")
		return false
	}
	return true
}

func TestCommittedUploadWithLostResponseCreatesOneObject(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		size    int
		foreign bool
	}{
		{name: "put", size: 5},
		{name: "multipart", size: 17 << 20},
		{name: "put with foreign identical audio", size: 5, foreign: true},
		{name: "multipart with foreign identical audio", size: 17 << 20, foreign: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, cfg := newIdentityStore(t)
			store.loseResponse = true
			r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
			data := bytes.Repeat([]byte{1}, tc.size)
			wantKey, wantObjects := uploadTestKey, 1
			if tc.foreign {
				store.objects[uploadTestKey] = storedTestUpload{id: "foreign", body: data}
				wantKey, wantObjects = uploadTestKey2, 2
			}
			store.beforeWrite = func(id string) { checkPersistedIdentity(t, r, id) }
			file := writeSpoolFile(t, r.spoolDir, r.id, filepath.Base(uploadTestKey), string(data))
			r.uploadFile(t.Context(), uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: int64(tc.size), deleteAfterUpload: true})
			// A failed CompleteMultipartUpload may need the outer retry after NoSuchUpload.
			r.processRetryQueue()
			store.mu.Lock()
			defer store.mu.Unlock()
			object := store.objects[wantKey]
			if len(store.objects) != wantObjects || !bytes.Equal(object.body, data) || object.id == "" || object.id == "foreign" {
				t.Fatalf("committed object keys = %v, want %s and %d objects", slices.Sorted(maps.Keys(store.objects)), wantKey, wantObjects)
			}
			if r.PendingUploadCount() != 0 {
				t.Fatal("committed upload is still pending")
			}
			if _, err := os.Stat(file); !os.IsNotExist(err) {
				t.Fatalf("completed S3-only spool file remains: %v", err)
			}
		})
	}
}

// A stop that interrupts an upload after S3 committed it must neither lose
// nor duplicate the recording: the retry recognizes the committed identity.
func TestUploadInterruptedAfterCommitIsNotDuplicated(t *testing.T) {
	t.Parallel()
	store := &identityStore{objects: make(map[string]storedTestUpload), multipart: make(map[string]*multipartTestUpload)}
	ctx, stop := context.WithCancel(t.Context())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut && ctx.Err() == nil {
			store.serveHTTP(httptest.NewRecorder(), req) // Committed; the response is lost.
			stop()
			<-req.Context().Done()
			return
		}
		store.serveHTTP(w, req)
	}))
	t.Cleanup(server.Close)
	cfg := testS3Recorder()
	cfg.S3Endpoint = server.URL
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
	file := writeSpoolFile(t, r.spoolDir, r.id, filepath.Base(uploadTestKey), "audio")
	r.uploadFile(ctx, uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: 5, deleteAfterUpload: true})
	if r.PendingUploadCount() != 1 || r.retryQueue[0].lastError != uploadStoppedReason {
		t.Fatalf("interrupted upload was not left for retry: %+v", r.retryQueue)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("interrupted upload removed its spool file: %v", err)
	}
	r.processRetryQueue()
	store.mu.Lock()
	keys := slices.Sorted(maps.Keys(store.objects))
	store.mu.Unlock()
	if !slices.Equal(keys, []string{uploadTestKey}) {
		t.Fatalf("stored keys = %v, want the interrupted upload once", keys)
	}
	assertUploadRecoveryRemoved(t, r, file)
}

func checkPersistedIdentity(t *testing.T, r *GenericRecorder, id string) {
	t.Helper()
	pending, err := r.loadPendingUploads()
	if err != nil || len(pending) != 1 {
		t.Errorf("identity was not durably stored before write: %v, %v", pending, err)
		return
	}
	if id == "" || pending[0].uploadID != id || pending[0].baseKey != uploadTestKey {
		t.Errorf("persisted identity = %+v, wire ID = %q", pending[0].request, id)
	}
}

func TestCommittedUploadIdentitySurvivesRestart(t *testing.T) {
	t.Parallel()
	for _, size := range []int{5, 17 << 20} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			t.Parallel()
			store, cfg := newIdentityStore(t)
			store.loseResponse, store.headStatus = true, http.StatusForbidden
			r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
			store.beforeWrite = func(id string) { checkPersistedIdentity(t, r, id) }
			data := bytes.Repeat([]byte{1}, size)
			file := writeSpoolFile(t, r.spoolDir, r.id, filepath.Base(uploadTestKey), string(data))
			r.uploadFile(t.Context(), uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: int64(size), deleteAfterUpload: true})
			if r.PendingUploadCount() != 1 {
				t.Fatal("unverified commit must remain queued")
			}
			original := r.retryQueue[0]
			store.mu.Lock()
			store.headStatus = 0
			store.mu.Unlock()
			cfg.Name = "Renamed after restart"
			restarted := restartManager(t, r.spoolDir, cfg)
			restored := restarted.recorders[cfg.ID]
			if restored.PendingUploadCount() != 1 || restored.retryQueue[0].uploadID != original.uploadID ||
				!restored.retryQueue[0].firstAttempt.Equal(original.firstAttempt) {
				t.Fatalf("upload identity/window changed on restart: %+v", restored.retryQueue)
			}
			restarted.processUploadRetryQueues()
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.objects) != 1 || store.objects[uploadTestKey].id != original.uploadID || restored.PendingUploadCount() != 0 {
				t.Fatalf("restart duplicated/lost upload: keys %v, pending %d", slices.Sorted(maps.Keys(store.objects)), restored.PendingUploadCount())
			}
			assertUploadRecoveryRemoved(t, restored, file)
		})
	}
}

func TestUploadRetrySuffixesOriginalKey(t *testing.T) {
	t.Parallel()
	for _, size := range []int{5, 17 << 20} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			t.Parallel()
			store, cfg := newIdentityStore(t)
			data := bytes.Repeat([]byte{1}, size)
			store.objects[uploadTestKey] = storedTestUpload{id: "foreign", body: data}
			store.rejectKey = uploadTestKey2
			r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
			store.beforeWrite = func(id string) { checkPersistedIdentity(t, r, id) }
			file := writeSpoolFile(t, r.spoolDir, r.id, filepath.Base(uploadTestKey), string(data))
			r.uploadFile(t.Context(), uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: int64(size), deleteAfterUpload: true})
			if r.PendingUploadCount() != 1 || r.retryQueue[0].request.s3Key != uploadTestKey2 {
				t.Fatalf("failed suffixed upload was not queued: %+v", r.retryQueue)
			}
			store.mu.Lock()
			store.objects[uploadTestKey2] = storedTestUpload{id: "another", body: data}
			store.rejectKey = ""
			store.mu.Unlock()
			restarted := restartManager(t, r.spoolDir, cfg)
			r = restarted.recorders[cfg.ID]
			r.processRetryQueue()
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.objects) != 3 || !bytes.Equal(store.objects[uploadTestKey3].body, data) {
				t.Fatalf("retried keys = %v, want original base with -3, never -2-2", slices.Sorted(maps.Keys(store.objects)))
			}
			assertUploadRecoveryRemoved(t, r, file)
		})
	}
}

func assertUploadRecoveryRemoved(t *testing.T, r *GenericRecorder, file string) {
	t.Helper()
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("completed S3-only file remains: %v", err)
	}
	entries, err := os.ReadDir(r.pendingUploadDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("stale/duplicate recovery metadata = %v, %v", entries, err)
	}
}

func TestUnverifiableConflictKeepsRecordingForRetry(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			store, cfg := newIdentityStore(t)
			store.headStatus = status
			store.objects[uploadTestKey] = storedTestUpload{id: "foreign", body: []byte("audio")}
			r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
			file := writeSpoolFile(t, r.spoolDir, r.id, filepath.Base(uploadTestKey), "audio")
			r.uploadFile(t.Context(), uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: 5, deleteAfterUpload: true})
			if r.PendingUploadCount() != 1 {
				t.Fatal("unverifiable conflict was not queued")
			}
			if data, err := os.ReadFile(file); err != nil || string(data) != "audio" { //nolint:gosec // Test temp file.
				t.Fatalf("unverified file = %q, %v, want preserved", data, err)
			}
			store.mu.Lock()
			if len(store.objects) != 1 || slices.Contains(store.keys, uploadTestKey2) {
				t.Error("unverified conflict caused a second upload")
			}
			store.headStatus = 0
			store.mu.Unlock()
			r.processRetryQueue()
			assertUploadRecoveryRemoved(t, r, file)
		})
	}
}

func TestUploadRequiresDurableIdentityBeforeNetworkWrite(t *testing.T) {
	t.Parallel()
	store, cfg := newIdentityStore(t)
	spool := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(spool, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: spool})
	file := filepath.Join(t.TempDir(), filepath.Base(uploadTestKey))
	if err := os.WriteFile(file, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.uploadFile(t.Context(), uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: 5, deleteAfterUpload: true})
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.keys) != 0 || r.PendingUploadCount() != 1 {
		t.Fatalf("unpersisted identity uploaded: writes %v, pending %d", store.keys, r.PendingUploadCount())
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "audio" { //nolint:gosec // Test temp file.
		t.Fatalf("unpersisted file = %q, %v, want preserved", data, err)
	}
}

func TestS3ConnectionRequiresReadableIdentityMetadata(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		strip  bool
		want   string
	}{
		{name: "supported"},
		{name: "no read permission", status: http.StatusForbidden, want: "HeadObject permission"},
		{name: "metadata stripped", strip: true, want: "does not preserve upload identity metadata"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, cfg := newIdentityStore(t)
			store.headStatus, store.stripIdentity = tc.status, tc.strip
			err := TestRecorderS3Connection(cfg)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("connection test = %v, want %q", err, tc.want)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.objects) != 0 {
				t.Fatal("connection test did not clean up its probe object")
			}
		})
	}
}

func TestLegacyUploadMetadataPreservesLocalCollisionSuffix(t *testing.T) {
	t.Parallel()
	for _, filename := range []string{filepath.Base(uploadTestKey), filepath.Base(uploadTestKey2)} {
		t.Run(filename, func(t *testing.T) {
			t.Parallel()
			r := NewGenericRecorder(GenericRecorderConfig{Recorder: testS3Recorder(), SpoolDir: t.TempDir()})
			file := writeSpoolFile(t, r.spoolDir, r.id, filename, "audio")
			base := "recordings/Original/" + filename
			metadata := pendingUploadMetadata{
				RecorderID: r.id, LocalPath: file, S3Key: withNumericSuffix(base, 3), FileSize: 5, DeleteAfterUpload: true,
			}
			data, err := json.Marshal(metadata) // No upload_id or base_s3_key in legacy metadata.
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(r.pendingUploadDir(), 0o700); err != nil {
				t.Fatal(err)
			}
			metadataPath := filepath.Join(r.pendingUploadDir(), "legacy.upload.json")
			if err := os.WriteFile(metadataPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			pending, ok, err := r.loadPendingUpload(metadataPath)
			if err != nil || !ok || pending.baseKey != base || pending.request.s3Key != metadata.S3Key {
				t.Fatalf("legacy upload = %+v, %t, %v; want base %s", pending, ok, err, base)
			}
			if err := r.savePendingUpload(&pending); err != nil {
				t.Fatal(err)
			}
			loaded, err := r.loadPendingUploads()
			if err != nil || len(loaded) != 1 || loaded[0].baseKey != base || loaded[0].uploadID == "" ||
				loaded[0].uploadID != pending.uploadID || loaded[0].metadataPath != metadataPath {
				t.Fatalf("migrated upload = %+v, %v; want stable identity/base/metadata path", loaded, err)
			}
		})
	}
}

func TestConcurrentRetryEnqueueKeepsOnePersistedIdentity(t *testing.T) {
	t.Parallel()
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: testS3Recorder(), SpoolDir: t.TempDir()})
	file := writeSpoolFile(t, r.spoolDir, r.id, filepath.Base(uploadTestKey), "audio")
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			r.enqueuePendingUpload(&pendingUpload{request: uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: 5}, lastError: "down"})
		})
	}
	wg.Wait()
	persisted, err := r.loadPendingUploads()
	if err != nil || len(persisted) != 1 || r.PendingUploadCount() != 1 || persisted[0].uploadID == "" ||
		persisted[0].uploadID != r.retryQueue[0].uploadID {
		t.Fatalf("concurrent enqueue changed/duplicated identity: persisted %+v, queued %+v, %v", persisted, r.retryQueue, err)
	}
}

func TestRetryEnqueueWhileUploadIsInFlightKeepsIdentity(t *testing.T) {
	t.Parallel()
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%t", fail), func(t *testing.T) {
			t.Parallel()
			store, cfg := newIdentityStore(t)
			if fail {
				store.rejectKey = uploadTestKey
			}
			r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
			file := writeSpoolFile(t, r.spoolDir, r.id, filepath.Base(uploadTestKey), "audio")
			req := uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: 5, deleteAfterUpload: true}
			r.addToRetryQueue(req, "down")
			originalID := r.retryQueue[0].uploadID
			type snapshot struct {
				persisted []pendingUpload
				err       error
			}
			seen := make(chan snapshot, 1)
			var once sync.Once
			// While the upload request is open, enqueue the same file again.
			store.beforeWrite = func(string) {
				once.Do(func() {
					r.addToRetryQueue(req, "duplicate while retrying")
					persisted, err := r.loadPendingUploads()
					seen <- snapshot{persisted, err}
				})
			}
			r.processRetryQueue()
			got := <-seen
			if got.err != nil || len(got.persisted) != 1 || got.persisted[0].uploadID != originalID {
				t.Fatalf("in-flight enqueue replaced identity: %+v, %v; want %s", got.persisted, got.err, originalID)
			}
			want := 0
			if fail {
				want = 1
			}
			if len(r.retryQueue) != want || fail && r.retryQueue[0].uploadID != originalID {
				t.Fatalf("queue after retry = %+v, want %d entries with original identity", r.retryQueue, want)
			}
			r.mu.RLock()
			remaining := len(r.retryInFlight)
			r.mu.RUnlock()
			if remaining != 0 {
				t.Fatal("completed batch retained in-flight ownership")
			}
		})
	}
}

func TestRetryRequestedDuringBatchRunsAfterIt(t *testing.T) {
	t.Parallel()
	store, cfg := newIdentityStore(t)
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
	file := writeSpoolFile(t, r.spoolDir, r.id, filepath.Base(uploadTestKey), "audio")
	r.addToRetryQueue(uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: 5, deleteAfterUpload: true}, "down")
	queued := writeSpoolFile(t, r.spoolDir, r.id, "queued-meanwhile.mp3", "later")
	var once sync.Once
	var overlapped atomic.Bool
	var queuedID string
	// While the first batch uploads, queue another file and ask for a retry pass.
	store.beforeWrite = func(id string) {
		once.Do(func() {
			r.addToRetryQueue(uploadRequest{localPath: queued, s3Key: "queued-meanwhile.mp3", fileSize: 5, deleteAfterUpload: true}, "queued meanwhile")
			r.mu.RLock()
			queuedID = r.retryQueue[0].uploadID
			r.mu.RUnlock()
			r.processRetryQueue()
		})
		if id == queuedID {
			r.mu.RLock()
			_, first := r.retryInFlight[file]
			r.mu.RUnlock()
			overlapped.Store(first)
		}
	}
	r.processRetryQueue()
	// PendingUploadCount reads zero while a batch has the queue drained, so
	// wait for the upload itself.
	uploaded := func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		_, ok := store.objects["queued-meanwhile.mp3"]
		return ok
	}
	for deadline := time.Now().Add(5 * time.Second); !uploaded() && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if !uploaded() {
		t.Fatal("file queued during a batch waited for the hourly retry")
	}
	if overlapped.Load() {
		t.Fatal("the follow-up pass ran while the first batch still owned its paths")
	}
}

func TestS3ConnectionLostPutResponseCleansOnlyOwnedProbe(t *testing.T) {
	t.Parallel()
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign=%t", foreign), func(t *testing.T) {
			t.Parallel()
			store, cfg := newIdentityStore(t)
			store.loseResponse, store.foreignIdentity = true, foreign
			err := TestRecorderS3Connection(cfg)
			if !foreign && err != nil || foreign && err == nil {
				t.Fatalf("connection test = %v, foreign=%t", err, foreign)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			want := 0
			if foreign {
				want = 1
			}
			if len(store.objects) != want {
				t.Fatalf("remaining objects = %v, want %d", store.objects, want)
			}
			if len(store.keys) < 2 {
				t.Fatal("test did not exercise the conditional PUT retry")
			}
			if store.headRequests != 1 {
				t.Fatalf("HEAD requests = %d, want reuse of the fresh verification result", store.headRequests)
			}
		})
	}
}

func TestMissingInitialUploadStillReportsFailure(t *testing.T) {
	t.Parallel()
	logger, loggedEvents := testEventLogger(t)
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: testS3Recorder(), SpoolDir: t.TempDir(), EventLogger: logger})
	file := filepath.Join(t.TempDir(), "missing.mp3")
	r.uploadFile(t.Context(), uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: 5})
	if r.PendingUploadCount() != 1 || !strings.Contains(r.retryQueue[0].lastError, "open recording for upload") {
		t.Fatalf("initial upload failure was swallowed: %+v", r.retryQueue)
	}
	if logged := loggedEvents(); !slices.Contains(logged, eventlog.UploadFailed) {
		t.Fatalf("upload failure was not logged: %v", logged)
	}
	// A later retry may clean up metadata for a file that really no longer exists.
	r.processRetryQueue()
	entries, err := os.ReadDir(r.pendingUploadDir())
	if err != nil || len(entries) != 0 || r.PendingUploadCount() != 0 {
		t.Fatalf("missing file retry left metadata: %v, %v, pending %d", entries, err, r.PendingUploadCount())
	}
}

// restartManager models an encoder restart that recovers the recorder's spool.
func restartManager(t *testing.T, spoolDir string, cfg *types.Recorder) *Manager {
	t.Helper()
	m, err := NewManager("", spoolDir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AddRecorder(cfg); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCompletedUploadKeepsIdentityWhileSpoolFileRemains(t *testing.T) {
	t.Parallel()
	endpoint, _ := conditionalPutServer(t, "audio", func(http.ResponseWriter, string) bool { return false })
	cfg := testS3Recorder()
	cfg.S3Endpoint = endpoint
	r := NewGenericRecorder(GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
	file := writeSpoolFile(t, r.spoolDir, r.id, "kept.mp3", "audio")
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("directory permissions do not stop root or Windows from deleting the file")
	}
	// A read-only spool directory makes deleting the uploaded copy fail.
	dir := filepath.Dir(file)
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // A test dir must stay traversable.
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // Restore the test dir for cleanup.
	p := pendingUpload{request: uploadRequest{localPath: file, s3Key: "recordings/Test/kept.mp3", fileSize: 5, deleteAfterUpload: true}}
	if !r.uploadPending(t.Context(), &p) {
		t.Fatal("upload failed")
	}
	if _, err := os.Stat(p.metadataPath); err != nil {
		t.Fatalf("upload identity removed while its spool file remains: %v", err)
	}
}
