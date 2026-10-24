package recording_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/recording"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// I2–I7 under a random schedule of starts, stops, updates, rotations, added
// and removed recorders, S3 faults and crashes. Every step is checked; the
// end checks that each fed chunk is exactly once where its recorder's
// settings sent it. Replay a failure with ZWFM_SCHEDULE_SEED; set the length
// with ZWFM_SCHEDULE_STEPS (default 256).
func TestInvariantSchedule(t *testing.T) {
	if !crashTests {
		t.Skip("crash steps need process groups")
	}
	seed := uint64(time.Now().UnixNano())
	if s, err := strconv.ParseUint(os.Getenv("ZWFM_SCHEDULE_SEED"), 10, 64); err == nil {
		seed = s
	}
	t.Logf("ZWFM_SCHEDULE_SEED=%d", seed)
	s3, spool, base := newFakeS3(t, "ok"), t.TempDir(), t.TempDir()
	progress := filepath.Join(t.TempDir(), "progress")
	for from := "0"; from != "done"; {
		runCrashChild(t, "schedule", true, spool, nil, "CORPUS_SEED="+strconv.FormatUint(seed, 10), "CORPUS_FROM="+from,
			"CORPUS_BASE="+base, "CORPUS_S3="+s3.url, "CORPUS_PROGRESS="+progress, "ZWFM_RECORDING_FAKE_FFMPEG_DROP_SILENCE=1")
		data, err := os.ReadFile(progress) //nolint:gosec // Test temp dir.
		if err != nil {
			t.Fatal(err)
		}
		from = string(data)
	}
}

// scheduleSteps holds each kind of step as often as it weighs.
var scheduleSteps = func() (steps []string) {
	for _, w := range []struct {
		name  string
		count int
	}{{"feed", 12}, {"rotate", 3}, {"start", 5}, {"stop", 2}, {"toggle", 2}, {"move", 1}, {"storage", 2},
		{"add", 2}, {"remove", 1}, {"s3", 2}, {"retry", 1}, {"stop-all", 1}, {"start-all", 3}, {"crash", 1}} {
		steps = append(steps, slices.Repeat([]string{w.name}, w.count)...)
	}
	return steps
}()

// schedule is the model of a running schedule: what each recorder's settings
// are, whether it records and where each chunk it was fed must end up.
type schedule struct {
	rng             *rand.Rand
	spool, base, s3 string
	m               *recording.Manager
	running         bool // The manager.
	recorders       []*scheduleRecorder
	added           int
}

type scheduleRecorder struct {
	cfg              types.Recorder
	started, removed bool // started: an on-demand recorder was started.
	moves            int  // LocalPath is <base>/<id>-<moves>.
	chunks           []scheduleChunk
}

// scheduleChunk is a chunk a recorder was fed and where it must end up.
type scheduleChunk struct {
	tag, dir string // dir: the local directory, "" in S3-only mode.
	s3       bool
}

func init() {
	recording.CorpusChildren["schedule"] = func() int {
		seed, _ := strconv.ParseUint(os.Getenv("CORPUS_SEED"), 10, 64)
		from, _ := strconv.Atoi(os.Getenv("CORPUS_FROM"))
		steps := 256
		if n, err := strconv.Atoi(os.Getenv("ZWFM_SCHEDULE_STEPS")); err == nil {
			steps = n
		}
		var err error
		s := &schedule{
			rng: rand.New(rand.NewPCG(seed, seed)), running: true, //nolint:gosec // A schedule must replay from its seed.
			spool: os.Getenv("CORPUS_SPOOL"), base: os.Getenv("CORPUS_BASE"), s3: os.Getenv("CORPUS_S3"),
		}
		s.add()
		s.add()
		for i := range steps {
			if i == from {
				err = s.open()
			}
			if err == nil {
				err = s.step(i, i >= from)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "schedule seed %d: %v\n", seed, err)
				return 1
			}
		}
		if from == steps {
			err = s.open()
		}
		if err == nil {
			err = s.finish()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "schedule seed %d, end: %v\n", seed, err)
			return 1
		}
		s.crash("done")
		return 0
	}
}

// open starts the manager on the spool with the model's recorders, as the
// service does at startup.
func (s *schedule) open() error {
	recording.ManualTimers(nil)
	m, err := recording.NewManager(os.Args[0], s.spool, 0, nil)
	if err != nil {
		return err
	}
	s.m = m
	m.StartUploads()
	for _, r := range s.live() {
		if err := s.apply(r, m.AddRecorder); err != nil {
			return err
		}
	}
	if s.running {
		err = m.Start()
	}
	recording.Settle(m)
	return errors.Join(err, s.check())
}

// step plays step i on the model and, when exec, on the manager, then checks.
func (s *schedule) step(i int, exec bool) error { //nolint:gocyclo // One case per kind of step.
	name := scheduleSteps[s.rng.IntN(len(scheduleSteps))]
	live := s.live()
	var r *scheduleRecorder
	if len(live) > 0 {
		r = live[s.rng.IntN(len(live))]
	}
	var do func() error
	switch {
	case name == "feed":
		tag := "c" + strconv.Itoa(i)
		var fed []*scheduleRecorder
		for _, r := range live {
			if s.records(r) {
				dir := r.cfg.LocalPath
				if r.cfg.StorageMode == types.StorageS3 {
					dir = ""
				}
				r.chunks = append(r.chunks, scheduleChunk{tag, dir, r.cfg.StorageMode != types.StorageLocal})
				fed = append(fed, r)
			}
		}
		do = func() error { return s.feed(tag, fed) }
	case name == "rotate":
		do = func() error { recording.FireTimers(s.m); return nil }
	case name == "start" && r != nil:
		ok := s.running && r.cfg.Enabled && r.cfg.RecordingMode == types.RecordingOnDemand && !r.started
		r.started = r.started || ok
		do = func() error { return want(ok, s.m.StartRecorder(r.cfg.ID)) }
	case name == "stop" && r != nil:
		ok := s.records(r) && r.cfg.RecordingMode == types.RecordingOnDemand
		r.started = r.started && !ok
		do = func() error { return s.bounded(func() error { return want(ok, s.m.StopRecorder(r.cfg.ID)) }) }
	case name == "toggle" && r != nil:
		r.cfg.Enabled = !r.cfg.Enabled
		r.started = r.started && r.cfg.Enabled
		do = func() error { return s.bounded(func() error { return s.apply(r, s.m.UpdateRecorder) }) }
	case name == "move" && r != nil:
		r.moves++
		r.cfg.LocalPath = filepath.Join(s.base, r.cfg.ID+"-"+strconv.Itoa(r.moves))
		do = func() error { return s.apply(r, s.m.UpdateRecorder) }
	case name == "storage" && r != nil:
		modes := []types.StorageMode{types.StorageLocal, types.StorageS3, types.StorageBoth}
		r.cfg.StorageMode = modes[(slices.Index(modes, r.cfg.StorageMode)+1+s.rng.IntN(2))%3]
		do = func() error { return s.apply(r, s.m.UpdateRecorder) }
	case name == "add" && len(live) < 3:
		r := s.add()
		do = func() error { return s.apply(r, s.m.AddRecorder) }
	case name == "remove" && r != nil:
		r.removed = true
		do = func() error { return s.bounded(func() error { return s.m.RemoveRecorder(r.cfg.ID) }) }
	case name == "s3":
		mode := []string{"ok", "deny", "hang", "conflict", "lose"}[s.rng.IntN(5)]
		do = func() error { return s.store(mode) }
	case name == "retry":
		do = func() error { recording.RetryUploadsNow(s.m); return nil }
	case name == "stop-all":
		s.running = false
		do = func() error { return s.bounded(s.m.Stop) }
	case name == "start-all":
		s.running = true
		do = func() error { return s.m.Start() }
	case name == "crash":
		s.running = true
		do = func() error { s.crash(strconv.Itoa(i + 1)); return nil }
	}
	if name == "stop-all" || name == "crash" {
		for _, r := range live {
			r.started = false
		}
	}
	if !exec || do == nil {
		return nil
	}
	err := do()
	recording.Settle(s.m)
	if err == nil {
		err = s.check()
	}
	if err != nil {
		return fmt.Errorf("step %d (%s): %w", i, name, err)
	}
	return nil
}

// add creates the model of a new recorder, hourly or on demand, with S3
// credentials and a local path whatever its storage mode.
func (s *schedule) add() *scheduleRecorder {
	s.added++
	id := "r" + strconv.Itoa(s.added)
	r := &scheduleRecorder{cfg: types.Recorder{
		ID: id, Name: id, Enabled: true, Codec: types.CodecMP3,
		RecordingMode: []types.RecordingMode{types.RecordingHourly, types.RecordingOnDemand}[s.rng.IntN(2)],
		StorageMode:   []types.StorageMode{types.StorageLocal, types.StorageS3, types.StorageBoth}[s.rng.IntN(3)],
		LocalPath:     filepath.Join(s.base, id+"-0"),
		S3Endpoint:    s.s3, S3Bucket: "bucket", S3AccessKeyID: "access", S3SecretAccessKey: "secret",
	}}
	s.recorders = append(s.recorders, r)
	return r
}

// apply hands a copy of r's settings to f, after creating its local path.
func (s *schedule) apply(r *scheduleRecorder, f func(*types.Recorder) error) error {
	cfg := r.cfg
	return errors.Join(os.MkdirAll(cfg.LocalPath, 0o750), f(&cfg))
}

func (s *schedule) live() []*scheduleRecorder {
	var live []*scheduleRecorder
	for _, r := range s.recorders {
		if !r.removed {
			live = append(live, r)
		}
	}
	return live
}

// records reports whether r must be recording.
func (s *schedule) records(r *scheduleRecorder) bool {
	return !r.removed && r.cfg.Enabled && s.running && (r.cfg.RecordingMode == types.RecordingHourly || r.started)
}

// feed writes one chunk and waits until each recorder fed it holds all of
// it, so nothing queues up and a crash loses none of it.
func (s *schedule) feed(tag string, fed []*scheduleRecorder) error {
	s.m.WriteAudio([]byte(payload(tag)))
	for _, r := range fed {
		dir := r.cfg.LocalPath
		if r.cfg.StorageMode == types.StorageS3 {
			dir = filepath.Join(s.spool, "recordings", r.cfg.RecordingFilePrefix())
		}
		if !waitFor(10*time.Second, func() bool { return fileHolds([]string{dir}, payload(tag)) }) {
			return fmt.Errorf("%s never wrote %s (drops %d)", r.cfg.ID, tag, s.m.Statuses()[r.cfg.ID].AudioDrops)
		}
	}
	return nil
}

func want(ok bool, err error) error {
	if (err == nil) != ok {
		return fmt.Errorf("error %w, want success %t", err, ok)
	}
	return nil
}

// bounded runs stop and fails when it takes longer than stopBound (I5).
func (s *schedule) bounded(stop func() error) error {
	took, inTime, err := stopWithin(stopBound, stop, func() { _ = s.store("ok") })
	if !inTime {
		return fmt.Errorf("stopping took %s, want at most %s", took, stopBound)
	}
	return err
}

// crash reports the step to resume at and hangs until the parent kills the
// process tree.
func (s *schedule) crash(next string) {
	if err := os.WriteFile(os.Getenv("CORPUS_PROGRESS"), []byte(next), 0o600); err != nil { //nolint:gosec // The parent's progress file.
		os.Exit(2)
	}
	fmt.Println("CRASH-POINT")
	select {}
}

// store switches the fake S3 to mode.
func (s *schedule) store(mode string) error {
	resp, err := http.Post(s.s3+"/mode", "text/plain", strings.NewReader(mode)) //nolint:gosec,noctx // The parent's fake S3.
	if err == nil {
		err = resp.Body.Close()
	}
	return err
}

// s3Tags counts the chunks in each recorder's S3 objects (I4: never twice).
func (s *schedule) s3Tags() (map[string]map[string]int, error) {
	resp, err := http.Get(s.s3 + "/objects") //nolint:gosec,noctx // The parent's fake S3.
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var bodies map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&bodies); err != nil {
		return nil, err
	}
	counts := map[string]map[string]int{}
	for _, r := range s.recorders {
		counts[r.cfg.ID] = map[string]int{}
		expected := map[string]bool{}
		for _, c := range r.chunks {
			expected[c.tag] = c.s3
		}
		for key, body := range bodies {
			if body == foreignBody || !strings.HasPrefix(key, "recordings/"+r.cfg.RecordingFilePrefix()+"/") {
				continue
			}
			tags, err := chunkTags(body)
			if err != nil {
				return nil, fmt.Errorf("S3 object %s: %w", key, err)
			}
			for _, tag := range tags {
				counts[r.cfg.ID][tag]++
				if !expected[tag] || counts[r.cfg.ID][tag] > 1 {
					return nil, fmt.Errorf("%s has unexpected or duplicate S3 chunk %s", r.cfg.ID, tag)
				}
			}
		}
	}
	return counts, nil
}

// check verifies the invariants that hold after every step: each recorder
// records exactly when its settings and the manager say so (I7), no chunk is
// in S3 twice or against its settings (I4), and a recorder that stopped with
// audio not in S3 has it counted as undelivered (I6).
func (s *schedule) check() error {
	statuses := s.m.Statuses()
	for _, r := range s.live() {
		if st := statuses[r.cfg.ID]; (st.State == types.ProcessRunning) != s.records(r) || st.Error != "" {
			return fmt.Errorf("%s is %s %q, want recording %t", r.cfg.ID, st.State, st.Error, s.records(r))
		}
	}
	backlog := undelivered(s.m) // Before S3 is read: S3 only gains objects.
	inS3, err := s.s3Tags()
	if err != nil {
		return err
	}
	waiting := 0
	for _, r := range s.recorders {
		lacks := false
		for _, c := range r.chunks {
			if inS3[r.cfg.ID][c.tag] == 0 && c.s3 && !s.records(r) {
				lacks = true
			}
		}
		if lacks {
			waiting++
		}
	}
	if backlog < waiting {
		return fmt.Errorf("%d recorders have recordings not in S3, but %d are undelivered", waiting, backlog)
	}
	return nil
}

// finish stops everything, delivers what waits and checks that every chunk
// is exactly once in each place its recorder's settings sent it (I2, I3, I4).
func (s *schedule) finish() error {
	if err := errors.Join(s.store("ok"), s.bounded(s.m.Stop)); err != nil {
		return err
	}
	waitFor(20*time.Second, func() bool {
		recording.RetryUploadsNow(s.m)
		return undelivered(s.m) == 0
	})
	s.m.Close()
	if tickets, _ := filepath.Glob(filepath.Join(s.spool, "outbox", "*.json")); len(tickets) > 0 {
		return fmt.Errorf("tickets left: %v", tickets)
	}
	inS3, err := s.s3Tags()
	if err != nil {
		return err
	}
	for _, r := range s.recorders {
		dirs := []string{filepath.Join(s.spool, "recordings", r.cfg.RecordingFilePrefix())}
		for move := range r.moves + 1 {
			dirs = append(dirs, filepath.Join(s.base, r.cfg.ID+"-"+strconv.Itoa(move)))
		}
		expectedDir := map[string]string{}
		for _, c := range r.chunks {
			expectedDir[c.tag] = c.dir
		}
		onDisk := map[string]map[string]int{}
		for _, dir := range dirs {
			onDisk[dir] = map[string]int{}
			files, _ := filepath.Glob(filepath.Join(dir, "*"))
			for _, file := range files {
				data, err := os.ReadFile(file) //nolint:gosec // Test temp dir.
				tags, parseErr := chunkTags(string(data))
				if err = errors.Join(err, parseErr); err != nil {
					return fmt.Errorf("%s: %w", file, err)
				}
				for _, tag := range tags {
					if expected, ok := expectedDir[tag]; !ok || expected != dir {
						return fmt.Errorf("unexpected chunk %s in %s", tag, file)
					}
					onDisk[dir][tag]++
				}
			}
		}
		for _, c := range r.chunks {
			if n := inS3[r.cfg.ID][c.tag]; n != b2i(c.s3) {
				return fmt.Errorf("%s has %s in S3 %d times, want %d", r.cfg.ID, c.tag, n, b2i(c.s3))
			}
			if n := onDisk[c.dir][c.tag]; c.dir != "" && n != 1 {
				return fmt.Errorf("%s has %s in %s %d times, want 1", r.cfg.ID, c.tag, c.dir, n)
			}
		}
	}
	return nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// chunkTags returns the tags of the chunks a recording holds, which must be
// whole and in the order they were fed.
func chunkTags(data string) ([]string, error) {
	var tags []string
	last := -1
	for data != "" {
		tag, _, _ := strings.Cut(data, "|")
		n, err := strconv.Atoi(strings.TrimPrefix(tag, "c"))
		if err != nil || n <= last || !strings.HasPrefix(data, payload(tag)) {
			return nil, fmt.Errorf("not whole chunks in feed order at %.30q", data)
		}
		tags, last, data = append(tags, tag), n, data[len(payload(tag)):]
	}
	return tags, nil
}
