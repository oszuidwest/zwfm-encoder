package recording

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

func TestLiveRenameKeepsStableNewObjectKeyAndAudio(t *testing.T) {
	store, cfg := newIdentityStore(t)
	cfg.Name, cfg.RecordingMode = "Before", types.RecordingOnDemand
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg, FFmpegPath: os.Args[0], SpoolDir: t.TempDir()})
	file := startCaptureTestRecorder(t, r)
	pcm := bytes.Repeat([]byte{1, 2, 3, 4}, 1024)
	r.WriteAudio(pcm)
	at := time.Now()
	before := r.generateFilename(at)
	updated := r.Config()
	updated.Name = strings.Repeat("Very long display name", 20)
	if err := r.UpdateConfig(&updated); err != nil {
		t.Fatal(err)
	}
	if after := r.generateFilename(at); after != before {
		t.Fatalf("rename changed recording identity: %q -> %q", before, after)
	}
	r.WriteAudio(pcm)
	r.Stop()
	deliverDue(t, r)
	key := "recordings/" + cfg.FilePrefix + "/" + filepath.Base(file)
	store.mu.Lock()
	defer store.mu.Unlock()
	object, ok := store.objects[key]
	if !ok || !bytes.Equal(object.body, append(pcm, pcm...)) || len(store.objects) != 1 {
		t.Fatalf("live rename lost or moved audio: %+v", store.objects)
	}
}

func TestRecorderRuntimeRejectsIdentityChange(t *testing.T) {
	t.Parallel()
	cfg := localOnDemandRecorder(t, "immutable")
	r := newTestRecorder(t, GenericRecorderConfig{Recorder: cfg})
	before := r.Config()
	updated := before
	updated.ID = "another"
	if err := r.UpdateConfig(&updated); err == nil || r.Config() != before {
		t.Fatal("runtime update changed the recorder's identity")
	}
}
