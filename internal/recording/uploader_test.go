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
	"testing"
	"time"

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

const (
	uploadTestKey  = "recordings/Test/Test-2026-09-28-14-37.mp3"
	uploadTestKey2 = "recordings/Test/Test-2026-09-28-14-37-2.mp3"
	uploadTestKey3 = "recordings/Test/Test-2026-09-28-14-37-3.mp3"
)

// readyTestTicket stores data as a finalized S3-only recording of r for
// uploadTestKey and returns its ready ticket.
func readyTestTicket(t *testing.T, r *GenericRecorder, data []byte) ticket {
	t.Helper()
	path := filepath.Join(r.spoolDir, "recordings", filepath.Base(uploadTestKey))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := r.Config()
	dest := destinationID(cfg.ID, cfg.S3Endpoint, cfg.S3Bucket)
	if err := r.outbox.saveDestination(dest, *RecorderToS3Config(&cfg)); err != nil {
		t.Fatal(err)
	}
	tk := ticket{
		RecorderID: cfg.ID, RecorderName: cfg.Name, Path: path, Dest: dest,
		BaseKey: uploadTestKey, Key: uploadTestKey, Codec: string(types.CodecMP3), DeleteAfterUpload: true,
	}
	if err := r.outbox.newTicket(&tk); err != nil {
		t.Fatal(err)
	}
	tk.State, tk.ReadyAt = ticketReady, time.Now()
	if err := r.outbox.update(&tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func testUploader(r *GenericRecorder) *uploader { return &uploader{outbox: r.outbox, now: time.Now} }

// checkPersistedIdentity reads the outbox on disk: the identity a write uses
// must be durable before the write.
func checkPersistedIdentity(t *testing.T, r *GenericRecorder, id string) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(r.outbox.dir, id+".*.json"))
	if id == "" || len(matches) != 1 {
		t.Errorf("identity %q was not durably stored before the write: %v", id, matches)
		return
	}
	var stored ticket
	data, err := os.ReadFile(matches[0]) //nolint:gosec // Test temp outbox.
	if err == nil {
		err = json.Unmarshal(data, &stored)
	}
	if err != nil || stored.BaseKey != uploadTestKey {
		t.Errorf("persisted ticket = %+v, %v", stored, err)
	}
}

// assertDelivered checks that file's upload finished: its S3-only spool copy
// and its ticket are gone.
func assertDelivered(t *testing.T, r *GenericRecorder, file string) {
	t.Helper()
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("completed S3-only file remains: %v", err)
	}
	if tk, ok := ticketFor(r.outbox, file); ok {
		t.Fatalf("delivered ticket remains: %+v", tk)
	}
}

func TestCommittedUploadWithLostResponseCreatesOneObject(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		size    int
		foreign bool
	}{
		{name: "multipart", size: 17 << 20},
		{name: "put with foreign identical audio", size: 5, foreign: true},
		{name: "multipart with foreign identical audio", size: 17 << 20, foreign: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, cfg := newIdentityStore(t)
			store.loseResponse = true
			r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
			data := bytes.Repeat([]byte{1}, tc.size)
			wantKey, wantObjects := uploadTestKey, 1
			if tc.foreign {
				store.objects[uploadTestKey] = storedTestUpload{id: "foreign", body: data}
				wantKey, wantObjects = uploadTestKey2, 2
			}
			store.beforeWrite = func(id string) { checkPersistedIdentity(t, r, id) }
			tk := readyTestTicket(t, r, data)
			testUploader(r).process(t.Context(), &tk)
			// A failed CompleteMultipartUpload may need the next attempt after NoSuchUpload.
			r.outbox.retryAllNow()
			deliverDue(t, r)
			store.mu.Lock()
			defer store.mu.Unlock()
			object := store.objects[wantKey]
			if len(store.objects) != wantObjects || !bytes.Equal(object.body, data) || object.id != tk.ID {
				t.Fatalf("committed object keys = %v, want %s and %d objects", slices.Sorted(maps.Keys(store.objects)), wantKey, wantObjects)
			}
			assertDelivered(t, r, tk.Path)
		})
	}
}

// A stop that interrupts an upload after S3 committed it must neither lose
// nor duplicate the recording: the retry recognizes the committed identity.
func TestUploadInterruptedAfterCommitIsNotDuplicated(t *testing.T) {
	t.Parallel()
	store, cfg := newIdentityStore(t)
	ctx, stop := context.WithCancel(t.Context())
	store.loseResponse = true                   // Committed; the response is lost.
	store.beforeWrite = func(string) { stop() } // The stop arrives meanwhile.
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
	tk := readyTestTicket(t, r, []byte("audio"))
	testUploader(r).process(ctx, &tk)
	if got, _ := ticketFor(r.outbox, tk.Path); got.State != ticketReady || got.Attempts != 0 {
		t.Fatalf("interrupted upload was not left for retry: %+v", got)
	}
	deliverDue(t, r)
	store.mu.Lock()
	keys := slices.Sorted(maps.Keys(store.objects))
	store.mu.Unlock()
	if !slices.Equal(keys, []string{uploadTestKey}) {
		t.Fatalf("stored keys = %v, want the interrupted upload once", keys)
	}
	assertDelivered(t, r, tk.Path)
}

// Close interrupts a multipart upload: its stored parts are aborted, and the
// recording stays ready for a retry with its identity and attempts.
func TestCloseAbortsMultipartUpload(t *testing.T) {
	t.Parallel()
	store := &identityStore{objects: make(map[string]storedTestUpload), multipart: make(map[string]*multipartTestUpload)}
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("partNumber") == "2" { // Held, so the upload is in flight at Close.
			select {
			case <-req.Context().Done():
			case <-release:
			}
			return
		}
		store.serveHTTP(w, req)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	cfg := testS3Recorder()
	cfg.S3Endpoint = server.URL
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg})
	tk := readyTestTicket(t, r, bytes.Repeat([]byte{1}, 17<<20))
	tk.Attempts = 4
	if err := r.outbox.update(&tk); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(os.Args[0], r.spoolDir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	m.StartUploads()
	if !WaitFor(10*time.Second, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return slices.ContainsFunc(slices.Collect(maps.Values(store.multipart)), func(u *multipartTestUpload) bool { return len(u.parts) > 0 })
	}) {
		t.Fatal("no part of the multipart upload was stored")
	}
	m.Close()
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.multipart) != 0 || len(store.objects) != 0 {
		t.Errorf("after Close: %d multipart uploads left open, %d objects", len(store.multipart), len(store.objects))
	}
	restarted, err := openOutbox(m.spoolDir, func(*ticket) {})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := ticketFor(restarted, tk.Path); got.ID != tk.ID || got.State != ticketReady || got.Attempts != tk.Attempts || got.Key != tk.Key {
		t.Errorf("ticket after Close = %+v, want it ready as before", got)
	}
}

// I10 across a restart: a recovered upload keeps its age and backoff, so it is
// overdue at once, waits for its next attempt and clears the alert on delivery.
func TestRecoveredUploadKeepsAgeAndBackoff(t *testing.T) {
	t.Parallel()
	store, cfg := newIdentityStore(t)
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg})
	tk := readyTestTicket(t, r, []byte("audio"))
	tk.ReadyAt, tk.Attempts, tk.NextAttempt = time.Now().Add(-3*time.Hour), 4, time.Now().Add(time.Hour)
	if err := r.outbox.update(&tk); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(os.Args[0], r.spoolDir, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	if _, due, wait := m.outbox.nextDue(tk.NextAttempt.Add(-time.Minute)); due || wait != time.Minute {
		t.Fatalf("recovered upload due = %t, wait = %v; want false, 1m", due, wait)
	}
	alerts := make(chan UploadsOverdue, 2)
	m.SetUploadsOverdueCallback(func(o UploadsOverdue) { alerts <- o })
	next := func() (o UploadsOverdue) {
		select {
		case o = <-alerts:
		case <-time.After(10 * time.Second):
			t.Fatal("no overdue transition")
		}
		return o
	}
	m.StartUploads()
	if o := next(); o.Count != 1 || !o.Oldest.Equal(tk.ReadyAt) {
		t.Errorf("first alert after restart = %+v, want the recording overdue since %v", o, tk.ReadyAt)
	}
	if got, _ := ticketFor(m.outbox, tk.Path); got.Attempts != tk.Attempts || !got.NextAttempt.Equal(tk.NextAttempt) {
		t.Errorf("recovered ticket = %+v, want attempts %d due at %v", got, tk.Attempts, tk.NextAttempt)
	}
	RetryUploadsNow(m)
	if o := next(); o.Count != 0 {
		t.Errorf("alert after delivery = %+v, want it cleared", o)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.objects[uploadTestKey].id != tk.ID {
		t.Errorf("stored objects = %v, want the recovered recording", slices.Sorted(maps.Keys(store.objects)))
	}
}

func TestCommittedUploadIdentitySurvivesRestart(t *testing.T) {
	t.Parallel()
	for _, size := range []int{5, 17 << 20} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			t.Parallel()
			store, cfg := newIdentityStore(t)
			store.loseResponse, store.headStatus = true, http.StatusForbidden
			r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
			tk := readyTestTicket(t, r, bytes.Repeat([]byte{1}, size))
			testUploader(r).process(t.Context(), &tk)
			if pendingTickets(r) != 1 {
				t.Fatal("an unverified commit must stay pending")
			}
			store.mu.Lock()
			store.headStatus = 0
			store.mu.Unlock()
			restarted := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, SpoolDir: r.spoolDir})
			restarted.outbox.retryAllNow()
			deliverDue(t, restarted)
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.objects) != 1 || store.objects[uploadTestKey].id != tk.ID {
				t.Fatalf("restart duplicated or lost the upload: keys %v", slices.Sorted(maps.Keys(store.objects)))
			}
			assertDelivered(t, restarted, tk.Path)
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
			r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
			tk := readyTestTicket(t, r, data)
			testUploader(r).process(t.Context(), &tk)
			if got, _ := ticketFor(r.outbox, tk.Path); got.State != ticketReady || got.Key != uploadTestKey2 {
				t.Fatalf("failed suffixed upload was not kept: %+v", got)
			}
			store.mu.Lock()
			store.objects[uploadTestKey2] = storedTestUpload{id: "another", body: data}
			store.rejectKey = ""
			store.mu.Unlock()
			restarted := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, SpoolDir: r.spoolDir})
			restarted.outbox.retryAllNow()
			deliverDue(t, restarted)
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.objects) != 3 || !bytes.Equal(store.objects[uploadTestKey3].body, data) {
				t.Fatalf("retried keys = %v, want original base with -3, never -2-2", slices.Sorted(maps.Keys(store.objects)))
			}
			assertDelivered(t, restarted, tk.Path)
		})
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
			r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
			tk := readyTestTicket(t, r, []byte("audio"))
			testUploader(r).process(t.Context(), &tk)
			if pendingTickets(r) != 1 {
				t.Fatal("unverifiable conflict was not kept")
			}
			assertRecordedCapture(t, tk.Path, []byte("audio"))
			store.mu.Lock()
			if len(store.objects) != 1 || slices.Contains(store.keys, uploadTestKey2) {
				t.Error("unverified conflict caused a second upload")
			}
			store.headStatus = 0
			store.mu.Unlock()
			r.outbox.retryAllNow()
			deliverDue(t, r)
			assertDelivered(t, r, tk.Path)
		})
	}
}

func TestDeliveredRecordingKeepsTicketWhileSpoolFileRemains(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("directory permissions do not stop root or Windows from deleting the file")
	}
	store, cfg := newIdentityStore(t)
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
	tk := readyTestTicket(t, r, []byte("audio"))
	dir := filepath.Dir(tk.Path)
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // A test dir must stay traversable.
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // Restore the test dir for cleanup.
	testUploader(r).process(t.Context(), &tk)
	if got, _ := ticketFor(r.outbox, tk.Path); got.State != ticketDelivered {
		t.Fatalf("ticket = %+v; want it kept as delivered while its spool copy remains", got)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // Restore the test dir.
		t.Fatal(err)
	}
	r.outbox.retryAllNow()
	deliverDue(t, r)
	assertDelivered(t, r, tk.Path)
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.objects) != 1 {
		t.Fatalf("cleanup uploaded again: %v", slices.Sorted(maps.Keys(store.objects)))
	}
}

func TestMissingRecordingIsVerifiedAgainstS3(t *testing.T) {
	t.Parallel()
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%t", committed), func(t *testing.T) {
			t.Parallel()
			store, cfg := newIdentityStore(t)
			r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
			tk := readyTestTicket(t, r, []byte("audio"))
			if committed { // Delivered before a crash that lost the ticket update.
				store.objects[uploadTestKey] = storedTestUpload{id: tk.ID, body: []byte("audio")}
			}
			if err := os.Remove(tk.Path); err != nil {
				t.Fatal(err)
			}
			testUploader(r).process(t.Context(), &tk)
			got, ok := ticketFor(r.outbox, tk.Path)
			if committed && ok || !committed && (got.State != ticketReady || got.Attempts != 1 || !got.NextAttempt.After(time.Now())) {
				t.Fatalf("ticket = %+v, %t; want delivered when committed, retry otherwise", got, ok)
			}
			if !committed {
				if err := os.WriteFile(tk.Path, []byte("audio"), 0o600); err != nil {
					t.Fatal(err)
				}
				r.outbox.retryAllNow()
				deliverDue(t, r)
				assertDelivered(t, r, tk.Path)
			}
		})
	}
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
			recorder := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
			tk := readyTestTicket(t, recorder, []byte("audio"))
			if err := testUploader(recorder).upload(t.Context(), &tk); (err != nil) != tt.wantErr {
				t.Fatalf("upload() error = %v, want error %t", err, tt.wantErr)
			}
			if tk.Key != tt.wantKey {
				t.Errorf("S3 key = %q, want %q", tk.Key, tt.wantKey)
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
	recorder := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
	tk := readyTestTicket(t, recorder, make([]byte, 17<<20))
	if err := testUploader(recorder).upload(t.Context(), &tk); err != nil {
		t.Fatalf("upload() error = %v", err)
	}
	if tk.Key != uploadTestKey2 {
		t.Errorf("S3 key = %q, want %q", tk.Key, uploadTestKey2)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	// The SDK retries the in-body error as a 500 before giving up on the key.
	if want := []string{uploadTestKey, uploadTestKey2}; !slices.Equal(slices.Compact(store.keys), want) {
		t.Errorf("completed keys = %v, want %v", store.keys, want)
	}
}

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

func TestUploadRefusesWhatIsNotARecording(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		prepare func(path string) error
		want    ticketState
	}{
		{name: "failed marker", prepare: func(path string) error {
			return os.WriteFile(path+failedMarkerSuffix, []byte("failed"), 0o600) // #nosec G703 -- Marker beside a test-owned temporary recording.
		}, want: ticketReady},
		{name: "emptied", prepare: func(path string) error { return os.Truncate(path, 0) }, want: ticketLost},
		{name: "symlink", prepare: func(path string) error {
			target := path + ".target"
			if err := os.Rename(path, target); err != nil { // #nosec G703 -- Test temp file.
				return err
			}
			return os.Symlink(target, path)
		}, want: ticketReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, cfg := newIdentityStore(t)
			r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, SpoolDir: t.TempDir()})
			tk := readyTestTicket(t, r, []byte("audio"))
			if err := tc.prepare(tk.Path); err != nil {
				t.Skipf("cannot prepare the file: %v", err)
			}
			testUploader(r).process(t.Context(), &tk)
			if got, _ := ticketFor(r.outbox, tk.Path); got.State != tc.want {
				t.Fatalf("ticket = %+v, want state %s", got, tc.want)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.objects) != 0 {
				t.Fatalf("uploaded %v", slices.Sorted(maps.Keys(store.objects)))
			}
		})
	}
}

func TestUploaderStopIsTerminal(t *testing.T) {
	for _, started := range []bool{false, true} {
		u := &uploader{outbox: &outbox{}, now: time.Now}
		if started {
			u.start(false)
		}
		u.stop()
		done := u.done
		u.start(false)
		t.Cleanup(u.stop)
		if u.done != done {
			t.Fatal("uploader restarted after stop")
		}
	}
}
