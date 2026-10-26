package recording

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/audio"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/ffmpeg"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

// reserveRecordingFile exclusively creates path; tests replace it to hold a
// reservation like an unresponsive network share.
var reserveRecordingFile = func(path string) (*os.File, error) {
	if err := util.ValidatePath("recording file", path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666) //nolint:gosec // Preserve FFmpeg's output permissions (restricted by umask/ACLs); path uses a validated directory and sanitized filename.
}

// checkRecordingPath lets lifecycle tests hold directory validation on a slow mount.
var checkRecordingPath = util.CheckPathWritable

// removeEmptyRecording lets tests reproduce a failed unlink without relying on mount permissions.
var removeEmptyRecording = os.Remove

// syncInterval is how often a running recording is synced to disk (D4);
// tests shorten it.
var syncInterval = 30 * time.Second

// writerDrainTimeout bounds graceful writer drain; tests shorten it.
var writerDrainTimeout = 5 * time.Second

// clockSynced and clockSyncPoll gate the first file on a synchronized clock;
// tests replace them.
var (
	clockSynced   = clockSynchronized
	clockSyncPoll = time.Second
)

// errRecorderStopped marks intentional stop/rotation cancellation.
var errRecorderStopped = errors.New("recorder stopped")

const (
	// recorderAudioBufferChunks bounds backlog to about 3s of audio.
	recorderAudioBufferChunks = 30
	// silenceGap is how long a segment may get no PCM before its writer fills
	// the outage with silence (D1); well above the capture chunk interval.
	silenceGap = time.Second
	// silenceStep is how often an open outage is topped up.
	silenceStep = 100 * time.Millisecond
	// frameBytes is one sample of every channel in the capture format.
	frameBytes = audio.Channels * 2
	// processStopTimeout bounds waiting before escalating to SIGTERM.
	processStopTimeout = 10 * time.Second
	// processKillTimeout bounds waiting after SIGTERM before SIGKILL.
	processKillTimeout = 2 * time.Second
)

// segment is one FFmpeg process and the file it writes.
type segment struct {
	cfg    types.Recorder // The settings the file is made with.
	file   string
	lock   *sync.Mutex // The preparation lock of the file's directory.
	output *recordingOutput
	result *ffmpeg.StartResult

	// Set when the loop takes the segment on.
	audioCh    chan []byte
	writerDone chan struct{}
	stopSync   func()
	hasAudio   bool // Input PCM reached FFmpeg; set by the writer, read after writerDone.
}

// prepareJob is what a helper needs to prepare a segment.
type prepareJob struct {
	cfg       types.Recorder
	at        time.Time
	dir       string
	lock      *sync.Mutex
	used      map[string]time.Time
	waitClock bool
}

// waitForClock delays the first file to keep boot-time clock errors out of
// filenames and retention dates. The job then takes the configuration of that
// moment. False means the start was cancelled meanwhile.
func (r *GenericRecorder) waitForClock(job *prepareJob) bool {
	warned := false
	for {
		synced := clockSynced()
		if !synced && !warned {
			slog.Warn("system clock is not synchronized, recording starts once it is", "id", r.id)
			warned = true
		}
		current := false
		r.do(func() {
			if current = job == r.run.next; !current {
				return
			}
			if !synced {
				r.run.err = "waiting for system clock synchronization"
			} else if warned {
				job.cfg, job.at = r.run.cfg, time.Now()
			}
		})
		if !current {
			return false
		}
		if synced {
			return true
		}
		time.Sleep(clockSyncPoll)
	}
}

// openSegment prepares the directory, reserves a free name and starts FFmpeg.
// It returns the reserved file also when FFmpeg did not start.
func (r *GenericRecorder) openSegment(job *prepareJob) (*segment, string, error) {
	output, err := r.reserveOutput(job)
	if err != nil {
		return nil, "", err
	}
	file := output.file.Name()
	var result *ffmpeg.StartResult
	err = errRecorderClosed
	select {
	case <-r.exited: // Removed while the reservation hung: start nothing.
	default:
		result, err = ffmpeg.StartProcess(r.ffmpegPath, encoderArgs(&job.cfg, file))
	}
	if err != nil {
		job.lock.Lock()
		output.discard()
		job.lock.Unlock()
		return nil, file, err
	}
	return &segment{cfg: job.cfg, file: file, lock: job.lock, output: output, result: result}, file, nil
}

// reserveOutput runs one at a time per directory: a check hung on a mount does
// not gain another blocked syscall with every start, and another directory is
// not held up.
func (r *GenericRecorder) reserveOutput(job *prepareJob) (*recordingOutput, error) {
	job.lock.Lock()
	defer job.lock.Unlock()
	if err := prepareRecordingDir(job.cfg.StorageMode, job.dir); err != nil {
		return nil, err
	}
	var output *recordingOutput
	var err error
	nextRecordingFilename(recordingFilename(&job.cfg, job.at), func(name string) bool {
		path := filepath.Join(job.dir, name)
		// A ticket keeps its name, also while its file is missing.
		if _, exists := job.used[name]; exists || r.outbox.hasPath(path) {
			return true
		}
		output, err = r.reserveRecordingOutput(path, &job.cfg)
		return errors.Is(err, os.ErrExist)
	})
	return output, err
}

func encoderArgs(cfg *types.Recorder, path string) []string {
	args := ffmpeg.BaseInputArgs()
	args = append(args, "-c:a")
	args = append(args, types.BuildCodecArgs(cfg.Codec, cfg.Bitrate)...)
	return append(args,
		"-f", cfg.Codec.Format(),
		"-flush_packets", "1", // Finished packets reach the kernel as they are muxed.
		"-hide_banner",
		"-loglevel", "warning",
		"-y", // This placeholder was exclusively created by this recorder.
		path,
	)
}

// abandon cleans up a segment the loop no longer wants: the process is
// killed and the reservation discarded.
func (r *GenericRecorder) abandon(seg *segment) {
	if seg == nil {
		return
	}
	seg.result.Cancel(errRecorderStopped)
	_ = seg.result.Wait()
	// The cleanup keeps the directory lock, so a hung share holds one call at most.
	seg.lock.Lock()
	defer seg.lock.Unlock()
	seg.output.discard()
}

func prepareRecordingDir(storageMode types.StorageMode, dir string) error {
	if err := util.ValidatePath("recording directory", dir); err != nil {
		return err
	}
	if storageMode == types.StorageS3 {
		// The directories must survive a power cut with the audio in them,
		// also when an earlier attempt created them but could not sync them.
		err := mkdirSynced(dir, 0o755)
		if err == nil {
			err = errors.Join(syncDir(filepath.Dir(dir)), syncDir(filepath.Dir(filepath.Dir(dir))))
		}
		if err != nil {
			return fmt.Errorf("failed to create spool directory: %w", err)
		}
		return nil
	}
	// CheckPathWritable logs the cause; its error adds nothing for the status.
	if checkRecordingPath(dir) != nil {
		return errors.New("local path is not writable")
	}
	return nil
}

// recordingOutput keeps the exclusive reservation open through finalization,
// so a removed/replaced path cannot be mistaken for this encoder's file.
// Recording directories must be writable only by trusted operators: FFmpeg
// reopens the reserved path, so arbitrary hostile directory mutation is unsupported.
// Its ticket belongs to the recorder until the output is committed.
type recordingOutput struct {
	file   *os.File
	outbox *outbox
	ticket ticket
}

// reserveRecordingOutput exclusively creates path and its ticket, so the
// recording can be found after a crash from its first byte on.
func (r *GenericRecorder) reserveRecordingOutput(path string, cfg *types.Recorder) (*recordingOutput, error) {
	file, err := reserveRecordingFile(path)
	if err != nil {
		return nil, fmt.Errorf("reserve recording file: %w", err)
	}
	t := ticket{
		RecorderID:   cfg.ID,
		RecorderName: cfg.Name,
		Path:         path,
		Codec:        string(cfg.Codec),
	}
	if cfg.StorageMode != types.StorageLocal && cfg.HasS3Credentials() {
		t.Dest = destinationID(cfg.ID, cfg.S3Endpoint, cfg.S3Bucket)
		t.BaseKey = s3Namespace(cfg.FilePrefix) + filepath.Base(path)
		t.Key = t.BaseKey
		t.DeleteAfterUpload = cfg.StorageMode == types.StorageS3
		if err := r.outbox.ensureDestination(t.Dest, *RecorderToS3Config(cfg)); err != nil {
			err = errors.Join(fmt.Errorf("save upload destination: %w", err), file.Close(), os.Remove(path)) // #nosec G703 -- The reservation this call created.
			return nil, err
		}
	} else if cfg.StorageMode != types.StorageLocal {
		slog.Warn("S3 not configured but storage mode requires it", "id", cfg.ID, "mode", cfg.StorageMode)
	}
	err = syncDir(filepath.Dir(path))
	if err == nil {
		err = r.outbox.newTicket(&t)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create recording ticket: %w", err), file.Close(), os.Remove(path)) // #nosec G703 -- The reservation this call created.
	}
	return &recordingOutput{file: file, outbox: r.outbox, ticket: t}, nil
}

// sync makes what the encoder wrote so far durable and records how much that
// is, so a restart after a power cut can tell lost audio from an empty file.
func (o *recordingOutput) sync() error {
	info, err := o.file.Stat()
	if err != nil {
		return err
	}
	if err := fsync(o.file); err != nil {
		return err
	}
	if info.Size() <= o.ticket.SyncedBytes {
		return nil
	}
	o.ticket.SyncedBytes = info.Size()
	return o.outbox.update(&o.ticket)
}

// commit hands a finalized, released output on: in local-only mode the file is
// the recording and its ticket goes; otherwise the uploader takes the ticket.
func (o *recordingOutput) commit() error {
	if o.ticket.Dest == "" {
		return o.outbox.remove(&o.ticket)
	}
	o.ticket.State, o.ticket.ReadyAt = ticketReady, time.Now()
	return o.outbox.update(&o.ticket)
}

// release closes the reservation, once, before any upload or removal (Windows
// cannot remove an open file). It returns the output size if the path still
// names the file this recorder created.
func (o *recordingOutput) release() (int64, error) {
	defer func() {
		if err := o.file.Close(); err != nil {
			slog.Warn("failed to close recording reservation", "file", o.file.Name(), "error", err)
		}
	}()
	owned, err := o.file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat recording output: %w", err)
	}
	info, err := os.Lstat(o.file.Name())
	if errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("recording output disappeared before finalization: %w", err)
	}
	if err != nil {
		return 0, fmt.Errorf("check recording output ownership: %w", err)
	}
	if !os.SameFile(owned, info) {
		return 0, errors.New("recording output was replaced; refusing to upload or remove it")
	}
	return info.Size(), nil
}

// remove deletes a released output, then its ticket; one that is already gone
// counts as removed. When the removal fails, the ticket keeps the file out of uploads.
func (o *recordingOutput) remove() error {
	if err := removeEmptyRecording(o.file.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove empty recording: %w", err)
	}
	return o.outbox.remove(&o.ticket)
}

// discard is for unstarted reservations; never remove an unrelated replacement.
func (o *recordingOutput) discard() {
	if _, err := o.release(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			_ = o.outbox.remove(&o.ticket) // Nothing was recorded into it.
			return
		}
		o.outbox.fail(&o.ticket, err) // A replacement must not pass for a crash leftover.
		slog.Warn("cannot discard recording reservation", "file", o.file.Name(), "error", err)
		return
	}
	if err := o.remove(); err != nil {
		o.outbox.fail(&o.ticket, err)
		slog.Warn("failed to discard recording reservation", "file", o.file.Name(), "error", err)
	}
}

// audioWriter owns blocking stdin writes for one FFmpeg process.
// While no PCM arrives for silenceGap it writes silence, so the file keeps
// pace with the clock through an input outage (D1). It does not correct
// drift between the capture clock and the wall clock.
func (r *GenericRecorder) audioWriter(seg *segment) {
	defer close(seg.writerDone)
	idleSince := time.Now() // When the writer last finished writing input PCM.
	var filled int64        // Silent frames written in the current outage.
	endGap := func() {
		if filled > 0 {
			slog.Info("filled an input gap with silence", "id", r.id, "file", filepath.Base(seg.file),
				"duration", time.Duration(filled)*time.Second/audio.SampleRate)
			filled = 0
		}
	}
	defer endGap()
	timer := time.NewTimer(silenceGap)
	defer timer.Stop()

	for {
		var pcm []byte
		select {
		case chunk, ok := <-seg.audioCh:
			if !ok {
				return
			}
			endGap()
			pcm = chunk // Whole frames: the encoder's distributor aligns them.
		case <-timer.C:
			if len(seg.audioCh) > 0 { // Input is queued, not missing.
				timer.Reset(silenceGap)
				continue
			}
			// At most a second per write, so a long stall (a blocked FFmpeg, a
			// resumed machine) catches up in steps instead of one allocation.
			frames := min(int64(time.Since(idleSince).Seconds()*audio.SampleRate)-filled, audio.SampleRate)
			filled += frames
			pcm = make([]byte, frames*frameBytes)
		}
		n, err := seg.result.WriteStdin(pcm)
		if err == nil {
			if filled == 0 {
				seg.hasAudio = seg.hasAudio || n > 0
				idleSince = time.Now()
				timer.Reset(silenceGap)
			} else {
				timer.Reset(silenceStep)
			}
			continue
		}

		// Stop and rotation intentionally close this path.
		if errors.Is(err, ffmpeg.ErrStdinClosed) ||
			errors.Is(context.Cause(seg.result.Context()), errRecorderStopped) {
			return
		}

		r.post(func() { r.writeFailed(seg, err) })
		return
	}
}

// startSync syncs seg every syncInterval until the returned function, which
// waits for a sync in progress, is called. A failed sync is a recorder
// warning; the synced length does not advance.
func (r *GenericRecorder) startSync(seg *segment) func() {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(syncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			if err := seg.output.sync(); err != nil {
				slog.Warn("cannot sync recording to disk", "id", r.id, "file", filepath.Base(seg.file), "error", err)
				r.post(func() {
					if r.run.seg == seg {
						r.run.err = "cannot sync recording to disk: " + err.Error()
					}
				})
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// finishEncoder finalizes seg and hands its file on. Stuck writers are
// cancelled instead of taking stdinMu.
func (r *GenericRecorder) finishEncoder(seg *segment, writeErr error) error {
	if writeErr != nil {
		seg.stopSync()
		// The failing write released stdinMu; CloseStdin cannot deadlock.
		seg.result.CloseStdin()
		if err := r.drainProcess(seg.result); err != nil {
			writeErr = err
		}
		return r.finishOutput(seg, writeErr)
	}

	// The hand-over closed the channel; the writer drains buffered chunks.
	var finalizeErr error
	select {
	case <-seg.writerDone:
		// Writer exited; stdin is idle, so EOF can finalize the file.
		seg.result.CloseStdin()
		finalizeErr = r.drainProcess(seg.result)
	case <-time.After(writerDrainTimeout):
		// WriteStdin holds stdinMu; cancel FFmpeg to unblock it.
		slog.Warn("recorder writer stuck, cancelling ffmpeg to break the pipe", "id", r.id, "file", filepath.Base(seg.file))
		seg.result.Cancel(errRecorderStopped)
		finalizeErr = errors.Join(fmt.Errorf("recording writer timed out after %s", writerDrainTimeout), r.drainProcess(seg.result))
		<-seg.writerDone
	}
	// After the writer, so a sync in progress cannot delay its drain; before
	// the final sync and the release close the file.
	seg.stopSync()

	// writerDone has fired; remaining buffered chunks are teardown loss.
	if abandoned := len(seg.audioCh); abandoned > 0 {
		drops := r.audioDrops.Add(int64(abandoned))
		slog.Warn("recorder abandoned buffered audio during teardown",
			"id", r.id, "file", filepath.Base(seg.file), "chunks", abandoned, "total_drops", drops)
	}

	if finalizeErr == nil {
		// The recording is never handed on before its bytes are durable.
		if err := fsync(seg.output.file); err != nil {
			finalizeErr = fmt.Errorf("sync recording to disk: %w", err)
		}
	}
	return r.finishOutput(seg, finalizeErr)
}

// finishOutput releases an output once, then discards, quarantines or commits it.
func (r *GenericRecorder) finishOutput(seg *segment, cause error) error {
	size, ownErr := seg.output.release()
	err := errors.Join(cause, ownErr)
	empty := !seg.hasAudio // Silence alone is not a recording (D1).
	if cause != nil {
		// A failed encoder can have written audio despite accepting no PCM.
		empty = size == 0
	}
	if ownErr == nil && empty {
		discardErr := seg.output.remove()
		if discardErr == nil {
			slog.Info("discarded recording without audio", "id", r.id, "file", filepath.Base(seg.file))
			return err
		}
		err = errors.Join(err, discardErr)
	}
	if err != nil {
		seg.output.outbox.fail(&seg.output.ticket, err)
		return err
	}
	if err := seg.output.commit(); err != nil {
		seg.output.outbox.publish(&seg.output.ticket)
		return fmt.Errorf("hand recording to upload: %w", err)
	}
	if seg.output.ticket.Dest == "" {
		slog.Info("recording saved", "id", r.id, "path", seg.file)
		return nil
	}
	slog.Info("queued file for upload", "id", r.id, "file", filepath.Base(seg.file))
	p := detailsOf(&seg.cfg)
	p.Filename, p.S3Key = filepath.Base(seg.file), seg.output.ticket.Key
	r.events.emit(eventlog.UploadQueued, p)
	return nil
}

// drainProcess waits for FFmpeg, escalating through SIGTERM and SIGKILL.
func (r *GenericRecorder) drainProcess(result *ffmpeg.StartResult) error {
	err := result.WaitEscalating(processStopTimeout, processKillTimeout,
		func() {
			slog.Warn("recorder ffmpeg did not stop in time, sending signal", "id", r.id)
		},
		func() { slog.Error("recorder ffmpeg force killed", "id", r.id) })

	if err != nil {
		if message := util.ExtractLastError(result.Stderr()); message != "" {
			return fmt.Errorf("%s: %w", message, err)
		}
	}
	return err
}
