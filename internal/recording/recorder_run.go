package recording

import (
	"log/slog"
	"maps"
	"path/filepath"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

// afterFunc schedules recorder timers; tests replace it to fire callbacks by hand.
var afterFunc = time.AfterFunc

const (
	// usedFilenameWindow is how long a started filename stays reserved. A
	// minute-based name only recurs within its minute or in the repeated hour
	// of a DST fall-back.
	usedFilenameWindow = 2 * time.Hour
)

// run is the recorder state the loop owns; only the loop goroutine touches it.
type run struct {
	cfg        types.Recorder
	maxMinutes int  // For on-demand mode (from global config).
	active     bool // The manager runs capture; starts are refused otherwise.
	state      types.ProcessState
	err        string // Stays until a later success clears it (#514).

	seg      *segment    // The encoder being written to.
	next     *prepareJob // The only preparation the loop accepts; nil when none.
	retiring int         // Segments being finalized; Stop waits for them.
	restart  bool        // Start again (hourly) once the stop in progress is done.
	stopped  chan struct{}

	// Timers act only while they are still the one in their field (see arm).
	hourly   *time.Timer // Next hour: rotate the current segment, or retry a failed run.
	duration *time.Timer // On-demand: the run's duration limit.

	usedFilenames map[string]time.Time   // Names started within usedFilenameWindow; S3-only mode deletes the file itself after upload.
	prepareLocks  map[string]*sync.Mutex // Serializes output preparation per directory; one entry per directory ever used.
}

// startRun begins a run: the first segment is prepared once the clock is
// synchronized.
func (r *GenericRecorder) startRun() error {
	l := &r.run
	switch l.state {
	case types.ProcessRunning, types.ProcessStarting:
		return ErrAlreadyRecording
	case types.ProcessStopping:
		return ErrRecorderStopping
	}
	stopTimer(&l.hourly)
	l.state = types.ProcessStarting
	r.audioDrops.Store(0)
	r.prepare(time.Now())
	return nil
}

// startHourly starts the recorder when it is enabled, hourly and active, also
// after an error; a stop in progress first finishes.
func (r *GenericRecorder) startHourly() {
	l := &r.run
	if !l.active || !l.cfg.Enabled || l.cfg.RecordingMode != types.RecordingHourly {
		return
	}
	if l.state == types.ProcessStopping {
		l.restart = true
		return
	}
	if l.state == types.ProcessStopped || l.state == types.ProcessError {
		_ = r.startRun()
	}
}

// stop ends the run and returns a channel that closes once every segment is
// finalized. An error from before the stop is cleared; one from finalizing
// stays.
func (r *GenericRecorder) stop() <-chan struct{} {
	l := &r.run
	l.restart = false
	if l.state == types.ProcessStopped || l.state == types.ProcessStopping {
		return l.stopped
	}
	l.stopped = make(chan struct{})
	r.endRun(types.ProcessStopping)
	l.err = ""
	if l.seg != nil {
		seg := l.seg
		l.seg = nil
		r.setAudio(nil)
		r.retire(seg, nil)
	}
	r.finishStop()
	return l.stopped
}

// endRun leaves the running states: capture stops, timers are cancelled and a
// segment in preparation becomes stale.
func (r *GenericRecorder) endRun(state types.ProcessState) {
	l := &r.run
	l.state = state
	r.recording.Store(false)
	l.next = nil
	stopTimer(&l.hourly)
	stopTimer(&l.duration)
}

// finishStop completes a stop once no segment is being finalized.
func (r *GenericRecorder) finishStop() {
	l := &r.run
	if l.state != types.ProcessStopping || l.retiring > 0 {
		return
	}
	l.state = types.ProcessStopped
	r.publishStatus() // Before a waiting Stop returns.
	close(l.stopped)
	slog.Info("recorder stopped", "id", r.id, "name", l.cfg.Name)
	r.events.emit(eventlog.RecorderStopped, detailsOf(&l.cfg))
	if l.restart {
		l.restart = false
		r.startHourly()
	}
}

// fail ends the run with err: a failed start, or an encoder that broke.
func (r *GenericRecorder) fail() {
	l := &r.run
	r.endRun(types.ProcessError)
	if l.cfg.RecordingMode != types.RecordingHourly {
		return
	}
	r.arm(&l.hourly, time.Until(nextHour(time.Now())), func() {
		slog.Info("retrying failed hourly recorder at hour boundary", "id", r.id, "name", l.cfg.Name)
		r.startHourly()
	})
}

// report makes err the status error and logs it as a recorder event.
func (r *GenericRecorder) report(file string, err error) {
	r.run.err = err.Error()
	p := detailsOf(&r.run.cfg)
	p.Error = err.Error()
	if file != "" {
		p.Filename = filepath.Base(file)
	}
	slog.Error("recorder error", "id", r.id, "file", p.Filename, "error", err)
	r.events.emit(eventlog.RecorderError, p)
}

func detailsOf(cfg *types.Recorder) *eventlog.RecorderDetails {
	return &eventlog.RecorderDetails{
		RecorderName: cfg.Name,
		Codec:        string(cfg.Codec),
		StorageMode:  string(cfg.StorageMode),
	}
}

// applyConfig makes cfg the recorder's configuration and applies it to the
// run: enabled and the recording mode right away, and new output settings by
// starting a new segment (D3).
func (r *GenericRecorder) applyConfig(cfg *types.Recorder) <-chan struct{} {
	l := &r.run
	old := l.cfg
	l.cfg = *cfg
	updated := l.cfg
	r.config.Store(&updated)

	hourly := l.cfg.RecordingMode == types.RecordingHourly
	if !l.cfg.Enabled || (!hourly && l.state == types.ProcessError) {
		// Saving clears a failed on-demand run, which only a start would reuse.
		return r.stop()
	}
	if l.state == types.ProcessRunning && hourly != (old.RecordingMode == types.RecordingHourly) {
		if hourly {
			stopTimer(&l.duration)
			r.scheduleRotation(time.Now())
		} else {
			// Leaving hourly mode cancels the pending rotation, also one
			// already preparing its next file.
			stopTimer(&l.hourly)
			l.next = nil
			r.armDurationLimit()
		}
	}
	if l.state == types.ProcessRunning || l.state == types.ProcessStarting {
		// A segment being prepared has the output of the previous settings;
		// without one, the current segment's output is what runs.
		planned := &old
		if l.next == nil {
			planned = &l.seg.cfg
		}
		if !sameOutput(planned, &l.cfg) {
			r.prepare(time.Now())
		}
	}
	r.startHourly()
	return closedChannel()
}

func sameOutput(a, b *types.Recorder) bool {
	return a.Codec == b.Codec && a.Bitrate == b.Bitrate &&
		a.StorageMode == b.StorageMode && a.LocalPath == b.LocalPath &&
		a.S3Endpoint == b.S3Endpoint && a.S3Bucket == b.S3Bucket &&
		a.HasS3Credentials() == b.HasS3Credentials()
}

// prepare starts preparing the next segment named after at; it supersedes one
// already being prepared. Only the first segment of a run waits for the clock.
func (r *GenericRecorder) prepare(at time.Time) {
	l := &r.run
	dir := l.cfg.LocalPath
	if l.cfg.StorageMode == types.StorageS3 {
		dir = filepath.Join(r.spoolDir, "recordings", "recorder-"+l.cfg.FilePrefix) // Never a bare Windows device name such as AUX.
	}
	lock := l.prepareLocks[dir]
	if lock == nil {
		lock = new(sync.Mutex)
		l.prepareLocks[dir] = lock
	}
	job := &prepareJob{cfg: l.cfg, at: at, dir: dir, lock: lock, used: maps.Clone(l.usedFilenames),
		waitClock: l.state == types.ProcessStarting}
	l.next = job
	r.helpers.Go(func() {
		if job.waitClock && !r.waitForClock(job) {
			return
		}
		seg, file, err := r.openSegment(job)
		if !r.post(func() { r.prepared(job, seg, file, err) }) {
			r.abandon(seg)
		}
	})
}

// prepared takes the result of job: seg, or the error preparing file.
func (r *GenericRecorder) prepared(job *prepareJob, seg *segment, file string, err error) {
	l := &r.run
	if job != l.next {
		r.helpers.Go(func() { r.abandon(seg) })
		return
	}
	l.next = nil
	if err != nil {
		if l.seg == nil { // The first segment of a run.
			slog.Error("recorder failed to start encoder", "id", r.id, "error", err)
			r.fail()
			r.report(file, err)
			return
		}
		// The current encoder still runs: keep recording into its file
		// instead of stopping until the hourly retry.
		r.report(file, err)
		if l.cfg.RecordingMode == types.RecordingHourly {
			r.scheduleRotation(job.at)
		}
		return
	}

	previous := l.seg
	r.begin(seg)
	if previous == nil {
		l.state = types.ProcessRunning
		r.recording.Store(true)
		slog.Info("recorder started", "id", r.id, "name", l.cfg.Name, "mode", l.cfg.RecordingMode)
		r.events.emit(eventlog.RecorderStarted, detailsOf(&l.cfg))
		if l.cfg.RecordingMode != types.RecordingHourly {
			r.armDurationLimit()
		}
	} else {
		slog.Info("recorder rotated file", "id", r.id, "from", filepath.Base(previous.file), "to", filepath.Base(seg.file))
		r.retire(previous, nil)
	}
	if l.cfg.RecordingMode == types.RecordingHourly {
		r.scheduleRotation(job.at)
	}
}

// begin makes seg the encoder capture writes to. Capture stays with the
// previous encoder until this one runs (make-before-break); the hand-over
// closes the previous channel, whose writer then drains it.
func (r *GenericRecorder) begin(seg *segment) {
	l := &r.run
	l.seg = seg
	rememberFilename(l.usedFilenames, filepath.Base(seg.file))
	l.err = "" // A successful start or rotation clears an earlier error.

	seg.audioCh = make(chan []byte, recorderAudioBufferChunks)
	seg.writerDone = make(chan struct{})
	r.setAudio(seg.audioCh)
	go r.audioWriter(seg)
	seg.stopSync = r.startSync(seg)

	slog.Info("recorder encoding started", "id", r.id, "file", filepath.Base(seg.file), "codec", seg.cfg.Codec)
	p := detailsOf(&seg.cfg)
	p.Filename = filepath.Base(seg.file)
	r.events.emit(eventlog.RecorderFile, p)
}

// retire finalizes seg in a helper and reports what went wrong.
func (r *GenericRecorder) retire(seg *segment, writeErr error) {
	r.run.retiring++
	r.helpers.Go(func() {
		err := r.finishEncoder(seg, writeErr)
		r.post(func() {
			r.run.retiring--
			if err != nil {
				r.report(seg.file, err)
			}
			r.finishStop()
		})
	})

}

// writeFailed handles an unexpected write error on seg's stdin: FFmpeg
// crashed or exited on its own.
func (r *GenericRecorder) writeFailed(seg *segment, err error) {
	l := &r.run
	if l.seg != seg {
		return // Stop or rotation finishes it.
	}
	l.seg = nil
	r.setAudio(nil)
	r.fail()
	l.err = err.Error()
	r.retire(seg, err)
}

func (r *GenericRecorder) scheduleRotation(from time.Time) {
	r.arm(&r.run.hourly, time.Until(nextHour(from)), r.rotate)
}

func (r *GenericRecorder) rotate() {
	// The wall clock names the file: a timer that fires a moment early still
	// gives HH-00, a late one its real start, and after a clock step the name
	// and the next boundary follow the corrected time.
	r.prepare(time.Now().Round(time.Minute))
}

func (r *GenericRecorder) armDurationLimit() {
	l := &r.run
	if l.maxMinutes <= 0 {
		return
	}
	limit := time.Duration(l.maxMinutes) * time.Minute
	r.arm(&l.duration, limit, func() {
		slog.Info("recorder max duration reached", "id", r.id, "duration", limit)
		r.stop()
	})
}

// arm sets *slot to a timer that runs f in the loop after d. Stopping or
// replacing the timer cancels f, also when the timer already fired and f
// waits for the loop.
func (r *GenericRecorder) arm(slot **time.Timer, d time.Duration, f func()) {
	stopTimer(slot)
	var t *time.Timer
	t = afterFunc(d, func() {
		r.post(func() {
			if *slot == t {
				*slot = nil
				f()
			}
		})
	})
	*slot = t
}

// rememberFilename reserves name and forgets names older than
// usedFilenameWindow.
func rememberFilename(used map[string]time.Time, name string) {
	now := time.Now()
	maps.DeleteFunc(used, func(_ string, started time.Time) bool {
		return now.Sub(started) > usedFilenameWindow
	})
	used[name] = now
}

func nextHour(from time.Time) time.Time {
	return from.Truncate(time.Hour).Add(time.Hour)
}

func stopTimer(t **time.Timer) {
	if *t != nil {
		(*t).Stop()
		*t = nil
	}
}
