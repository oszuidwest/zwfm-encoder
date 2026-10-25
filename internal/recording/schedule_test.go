package recording_test

import (
	"fmt"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/recording"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// Replay with ZWFM_SCHEDULE_SEED. Children resume on the same spool and S3;
// the parent audits all destinations, including removed and updated recorders.
func TestInvariantSchedule(t *testing.T) {
	if !crashTests {
		t.Skip("crash steps need process groups")
	}
	seed := os.Getenv("ZWFM_SCHEDULE_SEED")
	if seed == "" {
		seed = strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	t.Logf("ZWFM_SCHEDULE_SEED=%s", seed)
	t.Setenv("ZWFM_SCHEDULE_SEED", seed)
	s3 := newFakeS3(t, "deny")
	for key, value := range map[string]string{"CORPUS_BASE": t.TempDir(), "CORPUS_SPOOL": t.TempDir(), "CORPUS_S3": s3.url, "CORPUS_PROGRESS": filepath.Join(t.TempDir(), "progress")} {
		t.Setenv(key, value)
	}
	want := runSchedule(t, false)
	for from := "0"; from != "done"; {
		t.Setenv("CORPUS_FROM", from)
		runCrashChild(t, "schedule", true, os.Getenv("CORPUS_SPOOL"), nil)
		data, err := os.ReadFile(os.Getenv("CORPUS_PROGRESS")) //nolint:gosec // Test temp file.
		if err != nil {
			t.Fatal(err)
		}
		from = string(data)
	}
	s3.heal()
	m := newCorpusManager(t, os.Getenv("CORPUS_SPOOL"), s3)
	if !waitFor(20*time.Second, func() bool { recording.RetryUploadsNow(m); return undelivered(m) == 0 }) {
		t.Fatal("uploads did not drain")
	}
	m.Close()
	got := map[string]int{}
	audit := func(dir, body string) {
		// The real writer inserts silence during input outages.
		body = strings.ReplaceAll(body, "\x00", "")
		for body != "" {
			tag, _, _ := strings.Cut(body, "|")
			chunk := payload(tag)
			if !strings.HasPrefix(body, chunk) {
				t.Fatalf("partial chunk in %s: %.30q", dir, body)
			}
			got[dir+"/"+tag]++
			body = body[len(chunk):]
		}
	}
	for _, root := range []string{os.Getenv("CORPUS_BASE"), os.Getenv("CORPUS_SPOOL")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error { //nolint:gosec // Test temp roots.
			if err != nil || d.IsDir() || filepath.Ext(path) != ".mp3" {
				return err
			}
			data, err := os.ReadFile(path) //nolint:gosec // Test temp dir.
			audit(filepath.Dir(path), string(data))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	s3.set(func(s *fakeS3) {
		for key, object := range s.objects {
			audit(filepath.Dir(key), string(object.body))
		}
	})
	if !maps.Equal(got, want) {
		t.Fatalf("destination/chunk counts:\ngot %v\nwant %v", got, want)
	}
}
func init() {
	recording.CorpusChildren["schedule"] = func() int {
		testing.Main(func(_, _ string) (bool, error) { return true, nil }, []testing.InternalTest{{Name: "schedule", F: func(t *testing.T) { runSchedule(t, true) }}}, nil, nil)
		return 1 // Successful children wait at a crash point instead of returning.
	}
}

//nolint:gocyclo // Replays model transitions and executes only the resumed suffix.
func runSchedule(t *testing.T, execute bool) map[string]int {
	seed, err := strconv.ParseUint(os.Getenv("ZWFM_SCHEDULE_SEED"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(seed, seed)) //nolint:gosec // Replayable schedule.
	from, _ := strconv.Atoi(os.Getenv("CORPUS_FROM"))
	base, spool := os.Getenv("CORPUS_BASE"), os.Getenv("CORPUS_SPOOL")
	cfg := types.Recorder{ID: "schedule", Name: "schedule", Enabled: true, Codec: types.CodecMP3, RecordingMode: types.RecordingHourly, StorageMode: types.StorageBoth, LocalPath: base,
		S3Endpoint: os.Getenv("CORPUS_S3"), S3Bucket: "bucket", S3AccessKeyID: "access", S3SecretAccessKey: "secret"}
	want := map[string]int{}
	var m *recording.Manager
	check := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	crash := func(next string) {
		check(os.WriteFile(os.Getenv("CORPUS_PROGRESS"), []byte(next), 0o600)) //nolint:gosec // Parent's temp file.
		fmt.Println("CRASH-POINT")
		select {}
	}
	for i := range 128 {
		if execute && i == from {
			recording.ManualTimers(t)
			m = newCorpusManager(t, spool, nil, &cfg)
		}
		active := execute && i >= from
		switch rng.IntN(8) {
		case 0: // Change path and storage together.
			cfg.LocalPath = filepath.Join(base, strconv.Itoa(i))
			cfg.StorageMode = []types.StorageMode{types.StorageLocal, types.StorageS3, types.StorageBoth}[rng.IntN(3)]
			if active {
				check(os.MkdirAll(cfg.LocalPath, 0o750)) //nolint:gosec // Test temp dir.
				check(m.UpdateRecorder(&cfg))
			}
		case 1:
			if active {
				recording.FireTimers(m)
			}
		case 2:
			if i < 127 {
				if active {
					crash(strconv.Itoa(i + 1))
				}
				continue
			}
		}
		if active {
			recording.Settle(m)
		}
		tag := "c" + strconv.Itoa(i)
		if cfg.StorageMode != types.StorageS3 {
			want[cfg.LocalPath+"/"+tag]++
		}
		if cfg.StorageMode != types.StorageLocal {
			want["recordings/"+cfg.RecordingFilePrefix()+"/"+tag]++
		}
		if active {
			m.WriteAudio([]byte(payload(tag)))
			dir := cfg.LocalPath
			if cfg.StorageMode == types.StorageS3 {
				dir = filepath.Join(spool, "recordings", cfg.RecordingFilePrefix())
			}
			if !waitFor(5*time.Second, func() bool { return fileHolds([]string{dir}, payload(tag)) }) {
				t.Fatalf("step %d never wrote audio", i)
			}
		}
	}
	if execute {
		check(m.RemoveRecorder(cfg.ID)) // Pending delivery must survive removal.
		crash("done")
	}
	return want
}
