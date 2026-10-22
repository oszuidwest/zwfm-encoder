package recording_test

// Invariant corpus for the recording subsystem.
//
// Each scenario drives the recording Manager through its public API, a fake
// FFmpeg that copies PCM verbatim, a fake S3 store and the filesystem, and
// checks one guarantee the subsystem must give (see
// docs/recording-redesign.md). Scenarios do not look inside the package, so
// the same corpus judges the current code and a redesign.
import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/recording"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// undelivered counts recordings that are not in S3 and need no new audio.
func undelivered(m *recording.Manager) int {
	b := m.UploadBacklog()
	return b.Pending + b.Failed + b.Lost
}

// stopBound is how long stopping may take when nothing but the network is
// slow; the fake FFmpeg finalizes in 100ms.
const stopBound = 5 * time.Second

// fakeS3 stores objects under their key. Like a store with conditional
// writes, it refuses a PUT without If-None-Match: * and answers 412 when the
// key is taken. It counts committed writes per body, so a replaced or
// duplicated recording is visible.
type fakeS3 struct {
	url string

	mu           sync.Mutex
	mode         string // "ok", "deny" or "hang"
	release      chan struct{}
	held         int
	conflictOnce bool // The next PUT finds a foreign object at its key.
	loseResponse bool // The next committed PUT is answered with a 500.
	objects      map[string]storedObject
	writes       map[string]int
}

type storedObject struct {
	uploadID string
	body     []byte
}

const foreignBody = "foreign object"

func newFakeS3(t *testing.T, mode string) *fakeS3 {
	t.Helper()
	s := &fakeS3{
		mode: mode, release: make(chan struct{}),
		objects: make(map[string]storedObject), writes: make(map[string]int),
	}
	server := httptest.NewServer(s)
	s.url = server.URL
	t.Cleanup(func() {
		s.heal()
		server.Close()
	})
	return s
}

// hang holds every PUT until heal.
func (s *fakeS3) hang() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = "hang"
}

// heal makes the store accept writes again and lets held requests through.
func (s *fakeS3) heal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode == "hang" {
		close(s.release)
		s.release = make(chan struct{})
	}
	s.mode = "ok"
}

func (s *fakeS3) set(f func(s *fakeS3)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, code)
}

func (s *fakeS3) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	key, _ := strings.CutPrefix(req.URL.Path, "/bucket/")
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return
	}
	s.mu.Lock()
	if req.Method == http.MethodPut && s.mode == "hang" {
		s.held++
		release := s.release
		s.mu.Unlock()
		select {
		case <-release:
		case <-req.Context().Done():
		}
		s.mu.Lock()
		s.held--
		if req.Context().Err() != nil {
			s.mu.Unlock()
			return
		}
	}
	defer s.mu.Unlock()

	switch {
	case req.Method == http.MethodHead:
		object, ok := s.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("X-Amz-Meta-Zwfm-Upload-Id", object.uploadID)
		w.Header().Set("Content-Length", fmt.Sprint(len(object.body)))
	case req.Method != http.MethodPut:
		http.Error(w, "unexpected operation", http.StatusBadRequest)
	case req.Header.Get("If-None-Match") != "*":
		writeS3Error(w, http.StatusBadRequest, "UnconditionalWrite")
	case s.mode == "deny":
		writeS3Error(w, http.StatusForbidden, "AccessDenied")
	case s.conflictOnce:
		s.conflictOnce = false
		if _, exists := s.objects[key]; !exists {
			s.objects[key] = storedObject{uploadID: "someone-else", body: []byte(foreignBody)}
		}
		writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
	default:
		if _, exists := s.objects[key]; exists {
			writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		s.objects[key] = storedObject{uploadID: req.Header.Get("X-Amz-Meta-Zwfm-Upload-Id"), body: body}
		s.writes[string(body)]++
		if s.loseResponse {
			s.loseResponse = false
			writeS3Error(w, http.StatusInternalServerError, "InternalError")
			return
		}
		w.Header().Set("ETag", `"etag"`)
	}
}

func (s *fakeS3) heldRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
}

// count returns how many stored objects hold body.
func (s *fakeS3) count(body string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, o := range s.objects {
		if string(o.body) == body {
			n++
		}
	}
	return n
}

// committed returns how many PUTs wrote body.
func (s *fakeS3) committed(body string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes[body]
}

func corpusRecorder(t *testing.T, id string, storage types.StorageMode, mode types.RecordingMode, s3 *fakeS3) *types.Recorder {
	t.Helper()
	cfg := &types.Recorder{
		ID:            id,
		Name:          id,
		Enabled:       true,
		Codec:         types.CodecMP3,
		RecordingMode: mode,
		StorageMode:   storage,
	}
	if storage != types.StorageS3 {
		cfg.LocalPath = t.TempDir()
	}
	if storage != types.StorageLocal {
		cfg.S3Endpoint, cfg.S3Bucket, cfg.S3AccessKeyID, cfg.S3SecretAccessKey = s3.url, "bucket", "access", "secret"
	}
	return cfg
}

// newCorpusManager starts a Manager whose cleanup lets held S3 requests go
// before stopping, so a violating Stop cannot hang the test binary.
func newCorpusManager(t *testing.T, spool string, s3 *fakeS3, recorders ...*types.Recorder) *recording.Manager {
	t.Helper()
	m, err := recording.NewManager(os.Args[0], spool, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range recorders {
		if err := m.AddRecorder(cfg); err != nil {
			t.Fatal(err)
		}
	}
	m.StartUploads()
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s3 != nil {
			s3.heal()
		}
		_ = m.Stop()
		m.Close()
	})
	return m
}

var waitFor = recording.WaitFor

func state(m *recording.Manager, id string) types.ProcessState {
	return m.Statuses()[id].State
}

// record starts an on-demand recorder, feeds payload and returns once the
// recorder holds it; the caller stops it.
func record(t *testing.T, m *recording.Manager, id, payload string) {
	t.Helper()
	if err := m.StartRecorder(id); err != nil {
		t.Fatalf("StartRecorder: %v", err)
	}
	if !waitFor(5*time.Second, func() bool { return state(m, id) == types.ProcessRunning }) {
		t.Fatalf("recorder never ran: %+v", m.Statuses()[id])
	}
	feed(m, payload)
}

// feed writes payload in a few chunks, well below the recorder's 30-chunk
// buffer, so a correct recorder drops nothing.
func feed(m *recording.Manager, payload string) {
	for chunk := range slices.Chunk([]byte(payload), 512) {
		m.WriteAudio(chunk)
	}
	time.Sleep(100 * time.Millisecond) // Let the writer hand the chunks to FFmpeg.
}

// stopWithin runs stop and reports whether it returned within bound; it always
// waits for stop to return (after unblock) so no goroutine outlives the test.
func stopWithin(bound time.Duration, stop func() error, unblock func()) (took time.Duration, inTime bool, err error) {
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- stop() }()
	select {
	case err = <-done:
		return time.Since(start), true, err
	case <-time.After(bound):
		unblock()
		err = <-done
		return time.Since(start), false, err
	}
}

// audioFiles returns the content of every recording file (not markers or
// metadata) under dir.
func audioFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil || d.IsDir() || filepath.Ext(path) != ".mp3" {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // Test temp dir.
		files = append(files, string(data))
		return err
	})
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	return files
}

func payload(tag string) string { return strings.Repeat(tag+"|", 200) }

// I2, I4: every recorded byte reaches each configured destination unchanged
// and exactly once, and S3-only mode leaves no audio behind in the spool.
func TestInvariantRoundTrip(t *testing.T) {
	for _, storage := range []types.StorageMode{types.StorageLocal, types.StorageS3, types.StorageBoth} {
		t.Run("RoundTrip/"+string(storage), func(t *testing.T) {
			s3 := newFakeS3(t, "ok")
			spool := t.TempDir()
			cfg := corpusRecorder(t, "rt", storage, types.RecordingOnDemand, s3)
			m := newCorpusManager(t, spool, s3, cfg)
			want := payload("roundtrip")
			record(t, m, cfg.ID, want)
			if err := m.StopRecorder(cfg.ID); err != nil {
				t.Fatal(err)
			}
			if drops := m.Statuses()[cfg.ID].AudioDrops; drops != 0 {
				t.Errorf("recorder dropped %d chunks of paced audio", drops)
			}
			if storage != types.StorageS3 {
				if local := audioFiles(t, cfg.LocalPath); !slices.Equal(local, []string{want}) {
					t.Errorf("local files = %d, holding the recorded audio: %v", len(local), slices.Contains(local, want))
				}
			}
			if storage != types.StorageLocal {
				if !waitFor(5*time.Second, func() bool { return s3.count(want) == 1 }) {
					t.Errorf("S3 holds the recording %d times, want once", s3.count(want))
				}
				if n := s3.committed(want); n > 1 {
					t.Errorf("the recording was written to S3 %d times, want once", n)
				}
			}
			if storage == types.StorageS3 {
				if !waitFor(5*time.Second, func() bool { return len(audioFiles(t, spool)) == 0 }) {
					t.Errorf("spool still holds %d audio files after upload", len(audioFiles(t, spool)))
				}
			}
			if !waitFor(5*time.Second, func() bool { return m.UploadBacklog().Pending == 0 }) {
				t.Errorf("%d uploads pending after a successful upload", m.UploadBacklog().Pending)
			}
		})
	}
}

// I4: an upload never replaces an object it did not write, and a commit whose
// response is lost is not written twice.
func TestInvariantExactlyOnce(t *testing.T) {
	t.Run("ExactlyOnce/ForeignObject", func(t *testing.T) {
		s3 := newFakeS3(t, "ok")
		s3.set(func(s *fakeS3) { s.conflictOnce = true })
		cfg := corpusRecorder(t, "eo", types.StorageS3, types.RecordingOnDemand, s3)
		m := newCorpusManager(t, t.TempDir(), s3, cfg)
		want := payload("conflict")
		record(t, m, cfg.ID, want)
		if err := m.StopRecorder(cfg.ID); err != nil {
			t.Fatal(err)
		}
		if !waitFor(5*time.Second, func() bool { return s3.count(want) == 1 }) {
			t.Errorf("S3 holds the recording %d times next to a foreign object, want once", s3.count(want))
		}
		if n := s3.count(foreignBody); n != 1 {
			t.Errorf("the foreign object at the recording's key is gone (%d copies)", n)
		}
	})
	t.Run("ExactlyOnce/LostResponse", func(t *testing.T) {
		s3 := newFakeS3(t, "ok")
		s3.set(func(s *fakeS3) { s.loseResponse = true })
		cfg := corpusRecorder(t, "lr", types.StorageS3, types.RecordingOnDemand, s3)
		m := newCorpusManager(t, t.TempDir(), s3, cfg)
		want := payload("lost-response")
		record(t, m, cfg.ID, want)
		if err := m.StopRecorder(cfg.ID); err != nil {
			t.Fatal(err)
		}
		// Retry passes run at stop and on the hourly tick; run them until the
		// upload settles.
		waitFor(5*time.Second, func() bool {
			recording.RetryUploadsNow(m)
			return m.UploadBacklog().Pending == 0 && s3.count(want) > 0
		})
		if n, w := s3.count(want), s3.committed(want); n != 1 || w != 1 {
			t.Errorf("after a lost response S3 holds the recording %d times from %d writes, want 1 and 1", n, w)
		}
	})
}

// I5: stopping is bounded by local work; a slow or unreachable S3 never holds
// up a stop, so a service stop or input change cannot lose audio to it.
func TestInvariantBoundedStop(t *testing.T) {
	t.Run("BoundedStop/StopRecorder", func(t *testing.T) {
		s3 := newFakeS3(t, "hang")
		cfg := corpusRecorder(t, "bs", types.StorageS3, types.RecordingOnDemand, s3)
		m := newCorpusManager(t, t.TempDir(), s3, cfg)
		record(t, m, cfg.ID, payload("bounded"))
		took, inTime, err := stopWithin(stopBound, func() error { return m.StopRecorder(cfg.ID) }, s3.heal)
		if !inTime {
			t.Errorf("StopRecorder took %s with S3 unreachable, bound %s", took.Round(time.Millisecond), stopBound)
		}
		if err != nil {
			t.Errorf("StopRecorder: %v", err)
		}
	})
	t.Run("BoundedStop/ManagerStop", func(t *testing.T) {
		recording.ManualTimers(t)
		s3 := newFakeS3(t, "hang")
		cfg := corpusRecorder(t, "bm", types.StorageS3, types.RecordingHourly, s3)
		m := newCorpusManager(t, t.TempDir(), s3, cfg)
		if !waitFor(5*time.Second, func() bool { return state(m, cfg.ID) == types.ProcessRunning }) {
			t.Fatalf("hourly recorder never ran: %+v", m.Statuses()[cfg.ID])
		}
		feed(m, payload("hour-1"))
		recording.FireTimers(m) // Rotation queues hour 1 while S3 hangs.
		feed(m, payload("hour-2"))
		took, inTime, err := stopWithin(stopBound, m.Stop, s3.heal)
		if !inTime {
			t.Errorf("Manager.Stop took %s with an upload in flight, bound %s", took.Round(time.Millisecond), stopBound)
		}
		if err != nil {
			t.Errorf("Manager.Stop: %v", err)
		}
	})
}

// I6: a recording that is not yet in S3 is always counted as pending or
// abandoned, including while it uploads and after its recorder is removed.
func TestInvariantHonestPending(t *testing.T) {
	t.Run("HonestPending/UploadFailed", func(t *testing.T) {
		s3 := newFakeS3(t, "deny")
		cfg := corpusRecorder(t, "hp", types.StorageS3, types.RecordingOnDemand, s3)
		m := newCorpusManager(t, t.TempDir(), s3, cfg)
		record(t, m, cfg.ID, payload("denied"))
		if err := m.StopRecorder(cfg.ID); err != nil {
			t.Fatal(err)
		}
		if !waitFor(5*time.Second, func() bool { return undelivered(m) > 0 }) {
			t.Errorf("a refused upload is counted nowhere")
		}
	})
	t.Run("HonestPending/UploadInFlight", func(t *testing.T) {
		s3 := newFakeS3(t, "deny")
		cfg := corpusRecorder(t, "hi", types.StorageS3, types.RecordingOnDemand, s3)
		m := newCorpusManager(t, t.TempDir(), s3, cfg)
		record(t, m, cfg.ID, payload("inflight"))
		if err := m.StopRecorder(cfg.ID); err != nil {
			t.Fatal(err)
		}
		if !waitFor(5*time.Second, func() bool { return m.UploadBacklog().Pending > 0 }) {
			t.Fatal("precondition: the refused upload never became pending")
		}
		// Hold the next retry inside S3, then sample while it is in flight.
		s3.hang()
		var healed atomic.Bool
		retried := make(chan struct{})
		go func() {
			defer close(retried)
			for !healed.Load() && s3.heldRequests() == 0 {
				recording.RetryUploadsNow(m) // Returns at once while another pass runs.
				time.Sleep(20 * time.Millisecond)
			}
		}()
		if !waitFor(5*time.Second, func() bool { return s3.heldRequests() > 0 }) {
			healed.Store(true)
			s3.heal()
			<-retried
			t.Fatal("precondition: no retry upload reached S3")
		}
		zero := 0
		for range 50 {
			if undelivered(m) == 0 {
				zero++
			}
			time.Sleep(10 * time.Millisecond)
		}
		healed.Store(true)
		s3.heal()
		<-retried
		if zero > 0 {
			t.Errorf("pending+abandoned was 0 in %d of 50 samples while a retry upload was in flight", zero)
		}
	})
	t.Run("HonestPending/RemovedRecorder", func(t *testing.T) {
		s3 := newFakeS3(t, "deny")
		cfg := corpusRecorder(t, "hr", types.StorageS3, types.RecordingOnDemand, s3)
		m := newCorpusManager(t, t.TempDir(), s3, cfg)
		record(t, m, cfg.ID, payload("removed"))
		if err := m.StopRecorder(cfg.ID); err != nil {
			t.Fatal(err)
		}
		if !waitFor(5*time.Second, func() bool { return m.UploadBacklog().Pending > 0 }) {
			t.Fatal("precondition: the refused upload never became pending")
		}
		if err := m.RemoveRecorder(cfg.ID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second) // Give a background scan or retry pass time to count it.
		if undelivered(m) == 0 {
			t.Errorf("removing the recorder made its unuploaded recording invisible")
		}
		// R2: the removed recorder's recording is still delivered.
		s3.heal()
		if !waitFor(5*time.Second, func() bool {
			recording.RetryUploadsNow(m)
			return s3.count(payload("removed")) == 1 && undelivered(m) == 0
		}) {
			t.Errorf("the removed recorder's recording is in S3 %d times, want once", s3.count(payload("removed")))
		}
	})
}

// I10: an upload that keeps failing is retried until it is delivered; after
// two hours it is alerted as overdue, and the alert clears on delivery.
func TestInvariantOverdue(t *testing.T) {
	t.Run("Overdue/AlertedAndCleared", func(t *testing.T) {
		s3 := newFakeS3(t, "deny")
		cfg := corpusRecorder(t, "ov", types.StorageS3, types.RecordingOnDemand, s3)
		m, err := recording.NewManager(os.Args[0], t.TempDir(), 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var alerts []recording.UploadsOverdue
		m.SetUploadsOverdueCallback(func(o recording.UploadsOverdue) {
			mu.Lock()
			alerts = append(alerts, o)
			mu.Unlock()
		})
		var offset atomic.Int64
		recording.SetUploadClock(m, func() time.Time { return time.Now().Add(time.Duration(offset.Load())) })
		if err := m.AddRecorder(cfg); err != nil {
			t.Fatal(err)
		}
		m.StartUploads()
		if err := m.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			s3.heal()
			_ = m.Stop()
			m.Close()
		})
		alerted := func() []recording.UploadsOverdue {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(alerts)
		}
		record(t, m, cfg.ID, payload("overdue"))
		if err := m.StopRecorder(cfg.ID); err != nil {
			t.Fatal(err)
		}
		waitFor(5*time.Second, func() bool { return m.UploadBacklog().Pending == 1 })
		if len(alerted()) != 0 || m.UploadBacklog().Overdue != 0 {
			t.Errorf("a fresh failed upload is already overdue")
		}
		// Thirty hours later, past the old 24-hour abandonment as well.
		offset.Store(int64(30 * time.Hour))
		recording.RetryUploadsNow(m)
		if !waitFor(10*time.Second, func() bool { a := alerted(); return len(a) == 1 && a[0].Count == 1 }) {
			t.Errorf("an upload failing for 30 hours raised alerts %+v, want one overdue alert", alerted())
		}
		if b := m.UploadBacklog(); b.Overdue != 1 || b.Pending != 1 {
			t.Errorf("backlog after 30 hours = %+v, want one overdue pending upload", b)
		}
		s3.heal()
		if !waitFor(10*time.Second, func() bool {
			recording.RetryUploadsNow(m)
			return s3.count(payload("overdue")) == 1
		}) {
			t.Errorf("the overdue upload was not delivered once S3 healed")
		}
		if !waitFor(10*time.Second, func() bool { a := alerted(); return len(a) == 2 && a[1].Count == 0 }) {
			t.Errorf("alerts after delivery = %+v, want the overdue alert cleared once", alerted())
		}
	})
}

// I7: once the manager has stopped, no recorder runs until it starts again.
func TestInvariantClosedLifecycle(t *testing.T) {
	t.Run("ClosedLifecycle/StartAfterStop", func(t *testing.T) {
		cfg := corpusRecorder(t, "cl", types.StorageLocal, types.RecordingOnDemand, nil)
		m := newCorpusManager(t, t.TempDir(), nil, cfg)
		if err := m.Stop(); err != nil {
			t.Fatal(err)
		}
		_ = m.StartRecorder(cfg.ID)
		time.Sleep(time.Second)
		if s := state(m, cfg.ID); s == types.ProcessRunning || s == types.ProcessStarting {
			t.Errorf("recorder is %s after Manager.Stop", s)
		}
	})
}

// I7: a saved recorder configuration is what runs: enabling starts an hourly
// recorder, disabling stops it, and new output settings start a new file.
func TestInvariantConfigApplies(t *testing.T) {
	t.Run("ConfigApplies/Disable", func(t *testing.T) {
		recording.ManualTimers(t)
		cfg := corpusRecorder(t, "cd", types.StorageLocal, types.RecordingHourly, nil)
		m := newCorpusManager(t, t.TempDir(), nil, cfg)
		if !waitFor(5*time.Second, func() bool { return state(m, cfg.ID) == types.ProcessRunning }) {
			t.Fatalf("hourly recorder never ran")
		}
		updated := *cfg
		updated.Enabled = false
		if err := m.UpdateRecorder(&updated); err != nil {
			t.Fatal(err)
		}
		if !waitFor(stopBound, func() bool { return state(m, cfg.ID) == types.ProcessStopped }) {
			t.Errorf("disabled hourly recorder is still %s", state(m, cfg.ID))
		}
	})
	t.Run("ConfigApplies/Enable", func(t *testing.T) {
		recording.ManualTimers(t)
		cfg := corpusRecorder(t, "ce", types.StorageLocal, types.RecordingHourly, nil)
		cfg.Enabled = false
		m := newCorpusManager(t, t.TempDir(), nil, cfg)
		updated := *cfg
		updated.Enabled = true
		if err := m.UpdateRecorder(&updated); err != nil {
			t.Fatal(err)
		}
		if !waitFor(stopBound, func() bool { return state(m, cfg.ID) == types.ProcessRunning }) {
			t.Errorf("enabled hourly recorder is still %s", state(m, cfg.ID))
		}
	})
	t.Run("ConfigApplies/OutputSettings", func(t *testing.T) {
		recording.ManualTimers(t)
		cfg := corpusRecorder(t, "co", types.StorageLocal, types.RecordingHourly, nil)
		m := newCorpusManager(t, t.TempDir(), nil, cfg)
		if !waitFor(5*time.Second, func() bool { return state(m, cfg.ID) == types.ProcessRunning }) {
			t.Fatalf("hourly recorder never ran")
		}
		feed(m, payload("before"))
		updated := *cfg
		updated.Codec, updated.LocalPath = types.CodecPCM, t.TempDir()
		if err := m.UpdateRecorder(&updated); err != nil {
			t.Fatal(err)
		}
		recording.Settle(m)
		feed(m, payload("after"))
		if err := m.Stop(); err != nil {
			t.Fatal(err)
		}
		for _, want := range []struct{ dir, ext, data string }{
			{cfg.LocalPath, ".mp3", payload("before")},
			{updated.LocalPath, ".ts", payload("after")},
		} {
			files, _ := filepath.Glob(filepath.Join(want.dir, "*"))
			if len(files) != 1 || filepath.Ext(files[0]) != want.ext {
				t.Errorf("%s holds %v, want one %s recording", want.dir, files, want.ext)
				continue
			}
			if data, _ := os.ReadFile(files[0]); string(data) != want.data { //nolint:gosec // Test temp dir.
				t.Errorf("%s holds %d bytes, want the audio from its settings' time", files[0], len(data))
			}
		}
	})
}

// I12: through an input outage the file keeps pace with the clock (silence),
// and a segment that only got silence leaves nothing behind (D1).
func TestInvariantKeepsPace(t *testing.T) {
	const gap = 1300 * time.Millisecond
	t.Run("KeepsPace/Gaps", func(t *testing.T) {
		cfg := corpusRecorder(t, "kp", types.StorageLocal, types.RecordingOnDemand, nil)
		m := newCorpusManager(t, t.TempDir(), nil, cfg)
		record(t, m, cfg.ID, "")
		time.Sleep(gap)
		feed(m, payload("a"))
		time.Sleep(gap)
		feed(m, payload("b"))
		if err := m.StopRecorder(cfg.ID); err != nil {
			t.Fatal(err)
		}
		files, _ := filepath.Glob(filepath.Join(cfg.LocalPath, "*"))
		if len(files) != 1 {
			t.Fatalf("recordings = %v, want one", files)
		}
		data, _ := os.ReadFile(files[0]) //nolint:gosec // Test temp dir.
		a, b := bytes.Index(data, []byte(payload("a"))), bytes.Index(data, []byte(payload("b")))
		if a < 0 || b < a || b+len(payload("b")) != len(data) {
			t.Fatalf("recording does not hold both payloads in order: %d bytes", len(data))
		}
		for name, filler := range map[string][]byte{"leading": data[:a], "interior": data[a+len(payload("a")) : b]} {
			seconds := float64(len(filler)) / audio.BytesPerSecond
			if bytes.ContainsFunc(filler, func(r rune) bool { return r != 0 }) || seconds < gap.Seconds()-0.2 || seconds > gap.Seconds()+0.4 {
				t.Errorf("%s outage of about %s holds %.2f s, not all silence", name, gap, seconds)
			}
		}
	})
	for _, storage := range []types.StorageMode{types.StorageLocal, types.StorageS3} {
		t.Run("KeepsPace/SilenceOnly/"+string(storage), func(t *testing.T) {
			s3, spool := newFakeS3(t, "ok"), t.TempDir()
			cfg := corpusRecorder(t, "ks", storage, types.RecordingOnDemand, s3)
			m := newCorpusManager(t, spool, s3, cfg)
			record(t, m, cfg.ID, "")
			time.Sleep(gap)
			if err := m.StopRecorder(cfg.ID); err != nil {
				t.Fatal(err)
			}
			left, _ := filepath.Glob(filepath.Join(spool, "recordings", "*", "*"))
			if cfg.LocalPath != "" {
				left, _ = filepath.Glob(filepath.Join(cfg.LocalPath, "*"))
			}
			s3.mu.Lock()
			objects := len(s3.objects)
			s3.mu.Unlock()
			if len(left) != 0 || undelivered(m) != 0 || objects != 0 {
				t.Errorf("a silence-only segment left %v, %d undelivered, %d S3 objects", left, undelivered(m), objects)
			}
		})
	}
}

// I8: a recording that failed stays visible in the recorder status until the
// operator acts, also when the failure happened while stopping.
func TestInvariantErrorStaysVisible(t *testing.T) {
	t.Run("ErrorStaysVisible/FinalizeFailure", func(t *testing.T) {
		cfg := corpusRecorder(t, "ev", types.StorageLocal, types.RecordingOnDemand, nil)
		m := newCorpusManager(t, t.TempDir(), nil, cfg)
		t.Setenv("ZWFM_RECORDING_FAKE_FFMPEG_FAIL_FINALIZE", "1")
		record(t, m, cfg.ID, payload("failing"))
		if err := m.StopRecorder(cfg.ID); err != nil {
			t.Fatal(err)
		}
		if st := m.Statuses()[cfg.ID]; st.Error == "" && st.State != types.ProcessError {
			t.Errorf("finalize failure left status %q with no error", st.State)
		}
	})
}

// I3: a recording survives a crash: after restart, every file that holds
// audio reaches S3 exactly once. "service" kills the service process alone, so
// its FFmpeg children see stdin close and finalize; "tree" kills the whole
// process tree, FFmpeg included, as systemd's KillMode=mixed can. The fake
// FFmpeg streams its output, so a killed one leaves what it had written. Power
// loss is out of reach here.
func TestInvariantDurable(t *testing.T) {
	if !crashTests {
		t.Skip("crash scenarios need process groups")
	}
	for _, crash := range []string{"service", "tree"} {
		for _, storage := range []types.StorageMode{types.StorageS3, types.StorageBoth} {
			t.Run(crash+"-"+string(storage), func(t *testing.T) {
				s3 := newFakeS3(t, "hang")
				spool := t.TempDir()
				cfg := corpusRecorder(t, "du", storage, types.RecordingHourly, s3)
				runCrashChild(t, "queued-uploads", crash == "tree", spool, cfg)

				// Restart: the recorder is disabled so only recovery can upload.
				s3.heal()
				cfg.Enabled = false
				m := newCorpusManager(t, spool, s3, cfg)
				hours := []string{payload("hour-1"), payload("hour-2"), payload("hour-3"), payload("hour-4")}
				waitFor(10*time.Second, func() bool {
					recording.RetryUploadsNow(m)
					return !slices.ContainsFunc(hours, func(h string) bool { return s3.count(h) == 0 })
				})
				delivered := func(t *testing.T, numbers ...int) {
					for _, n := range numbers {
						if got := s3.count(hours[n-1]); got != 1 {
							t.Errorf("hour %d is in S3 %d times after restart, want once", n, got)
						}
					}
				}
				prefix := "Durable/" + crash + "/"
				t.Run(prefix+"QueuedFinalized/"+string(storage), func(t *testing.T) {
					delivered(t, 1, 2, 3) // Finalized: one in flight, two queued behind it.
				})
				t.Run(prefix+"InterruptedActive/"+string(storage), func(t *testing.T) {
					delivered(t, 4) // Still being recorded at the crash.
				})
			})
		}
	}
}

// runCrashChild runs a corpus child scenario and SIGKILLs it once it reports
// its crash point: the child alone, or its whole process tree. It returns
// once every process of the tree has exited.
func runCrashChild(t *testing.T, scenario string, tree bool, spool string, cfg *types.Recorder) {
	t.Helper()
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$") //nolint:gosec // Re-executes this test binary.
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "ZWFM_RECORDING_FAKE_FFMPEG=")
	})
	cmd.Env = append(cmd.Env, recording.CorpusChildEnv+"="+scenario, "CORPUS_SPOOL="+spool,
		"CORPUS_RECORDER="+string(cfgJSON), "ZWFM_RECORDING_FAKE_FFMPEG_STREAM=1")
	ownGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "CRASH-POINT" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	var reached bool
	select {
	case reached = <-ready:
	case <-ctx.Done():
	}
	if tree && reached {
		killGroup(cmd.Process.Pid)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if !reached {
		killGroup(cmd.Process.Pid)
		t.Fatalf("child never reached its crash point: %s", stderr.String())
	}
	if !waitGroupGone(cmd.Process.Pid, 10*time.Second) {
		t.Fatal("FFmpeg children of the killed service did not exit")
	}
}

func init() {
	// Records hours 1-3 through rotations while S3 hangs, so one upload is in
	// flight and the rest are queued, then reports the crash point during hour 4.
	recording.CorpusChildren["queued-uploads"] = func() int {
		var cfg types.Recorder
		if err := json.Unmarshal([]byte(os.Getenv("CORPUS_RECORDER")), &cfg); err != nil {
			return 2
		}
		recording.ManualTimers(nil)
		m, err := recording.NewManager(os.Args[0], os.Getenv("CORPUS_SPOOL"), 0, nil)
		if err != nil {
			return 2
		}
		m.StartUploads()
		if m.AddRecorder(&cfg) != nil || m.Start() != nil {
			return 2
		}
		if !waitFor(10*time.Second, func() bool { return state(m, cfg.ID) == types.ProcessRunning }) {
			return 3
		}
		for hour := 1; hour <= 3; hour++ {
			feed(m, payload(fmt.Sprintf("hour-%d", hour)))
			recording.FireTimers(m)
		}
		feed(m, payload("hour-4"))
		// Crash only once the streaming FFmpeg has written all of hour 4.
		dirs := []string{os.Getenv("CORPUS_SPOOL"), cfg.LocalPath}
		if !waitFor(10*time.Second, func() bool { return fileHolds(dirs, payload("hour-4")) }) {
			return 4
		}
		fmt.Println("CRASH-POINT")
		select {}
	}
}

// fileHolds reports whether a recording file under one of dirs holds exactly
// want.
func fileHolds(dirs []string, want string) bool {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		found := false
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error { //nolint:gosec // Temp dirs handed over by the parent test.
			if err != nil || d.IsDir() || filepath.Ext(path) != ".mp3" {
				return nil //nolint:nilerr // Keep looking; the file may still appear.
			}
			if data, err := os.ReadFile(path); err == nil && string(data) == want { //nolint:gosec // Test temp dir.
				found = true
				return fs.SkipAll
			}
			return nil
		})
		if found {
			return true
		}
	}
	return false
}
