package recording

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

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
	mu             sync.Mutex
	objects        map[string]storedTestUpload
	multipart      map[string]*multipartTestUpload
	nextID         int
	loseResponse   bool
	headStatus     int
	rejectKey      string
	stripIdentity  bool
	inBodyConflict bool
	beforeWrite    func(string)
	keys           []string
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
			r.uploadFile(uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: int64(tc.size), deleteAfterUpload: true})
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
			r.uploadFile(uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: int64(size), deleteAfterUpload: true})
			if r.PendingUploadCount() != 1 {
				t.Fatal("unverified commit must remain queued")
			}
			original := r.retryQueue[0]
			store.mu.Lock()
			store.headStatus = 0
			store.mu.Unlock()
			cfg.Name = "Renamed after restart"
			restarted, err := NewManager("", r.spoolDir, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.AddRecorder(cfg); err != nil {
				t.Fatal(err)
			}
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
			r.uploadFile(uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: int64(size), deleteAfterUpload: true})
			if r.PendingUploadCount() != 1 || r.retryQueue[0].request.s3Key != uploadTestKey2 {
				t.Fatalf("failed suffixed upload was not queued: %+v", r.retryQueue)
			}
			store.mu.Lock()
			store.objects[uploadTestKey2] = storedTestUpload{id: "another", body: data}
			store.rejectKey = ""
			store.mu.Unlock()
			restarted, err := NewManager("", r.spoolDir, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.AddRecorder(cfg); err != nil {
				t.Fatal(err)
			}
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
			r.uploadFile(uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: 5, deleteAfterUpload: true})
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
	r.uploadFile(uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: 5, deleteAfterUpload: true})
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
			r.addToRetryQueue(&pendingUpload{request: uploadRequest{localPath: file, s3Key: uploadTestKey, fileSize: 5}, lastError: "down"})
		})
	}
	wg.Wait()
	persisted, err := r.loadPendingUploads()
	if err != nil || len(persisted) != 1 || r.PendingUploadCount() != 1 || persisted[0].uploadID == "" ||
		persisted[0].uploadID != r.retryQueue[0].uploadID {
		t.Fatalf("concurrent enqueue changed/duplicated identity: persisted %+v, queued %+v, %v", persisted, r.retryQueue, err)
	}
}
