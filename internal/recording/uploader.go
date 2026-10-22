package recording

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oszuidwest/zwfm-encoder/internal/eventlog"
	"github.com/oszuidwest/zwfm-encoder/internal/types"
)

const (
	// uploadOverdueAfter is how long a finalized recording may wait for S3
	// before it is alerted as overdue (D2).
	uploadOverdueAfter = 2 * time.Hour
	// maxUploadBackoff caps the wait between attempts; uploads never give up.
	maxUploadBackoff = time.Hour
	// uploaderStopTimeout bounds Close: a cancelled multipart upload is aborted
	// within the transfer manager's FailTimeout (5s).
	uploaderStopTimeout = 10 * time.Second
	// maxS3KeySuffix bounds the suffixed keys an upload tries; each attempt
	// sends the whole file.
	maxS3KeySuffix = 10
)

// UploadsOverdue describes the recordings that wait longer than two hours
// for S3, or reports with Overdue false that none do any longer.
type UploadsOverdue struct {
	Overdue   bool
	Count     int
	Oldest    time.Time // When the oldest overdue recording became ready.
	Recorders []string
	LastError string
}

// uploader delivers ready tickets to S3, one at a time; each transfer already
// sends up to five parts in parallel. It runs from StartUploads until Close.
type uploader struct {
	outbox      *outbox
	eventLogger *eventlog.Logger
	refresh     func() // Stores current credentials again; runs hourly.

	overdueMu sync.Mutex // Serializes overdue transitions, so a clear never overtakes its alert.

	mu          sync.Mutex
	now         func() time.Time // Tests replace it.
	onOverdue   func(UploadsOverdue)
	overdueOpen bool
	cancel      context.CancelFunc
	done        chan struct{}
}

func (u *uploader) clock() time.Time {
	u.mu.Lock()
	now := u.now
	u.mu.Unlock()
	return now()
}

func (u *uploader) start(overdueOpen bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cancel != nil {
		return
	}
	u.overdueOpen = overdueOpen
	ctx, cancel := context.WithCancel(context.Background())
	u.cancel, u.done = cancel, make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() { u.deliver(ctx) })
	wg.Go(func() { u.watchOverdue(ctx) })
	go func() {
		wg.Wait()
		close(u.done)
	}()
}

// stop cancels uploads and waits for them; an interrupted upload stays ready.
func (u *uploader) stop() {
	u.mu.Lock()
	cancel, done := u.cancel, u.done
	u.cancel = nil
	u.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(uploaderStopTimeout):
		slog.Warn("recording uploads did not stop in time")
	}
}

func (u *uploader) deliver(ctx context.Context) {
	for ctx.Err() == nil {
		t, due, wait := u.outbox.nextDue(u.clock())
		if due {
			u.process(ctx, &t)
			u.checkOverdue()
			continue
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-u.outbox.wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// watchOverdue re-evaluates overdue uploads while an upload may be blocked,
// and refreshes the stored credentials every hour.
func (u *uploader) watchOverdue(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for minutes := 1; ; minutes++ {
		u.checkOverdue()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if minutes%60 == 0 && u.refresh != nil {
			u.refresh()
		}
	}
}

func (u *uploader) process(ctx context.Context, t *ticket) {
	if t.State == ticketDelivered {
		u.cleanup(t)
		return
	}
	if t.Attempts > 0 {
		u.logEvent(eventlog.UploadRetry, t, "")
	}
	err := u.upload(ctx, t)
	switch {
	case err == nil:
		t.State, t.LastError = ticketDelivered, ""
		u.save(t) // If this is lost, a restart verifies the object again.
		slog.Info("upload completed", "id", t.RecorderID, "s3_key", t.Key)
		u.logEvent(eventlog.UploadCompleted, t, "")
		u.cleanup(t)
	case errors.Is(err, errRecordingLost):
		t.State, t.LastError = ticketLost, err.Error()
		u.save(t)
		slog.Error("recording is gone before its upload", "id", t.RecorderID, "path", t.Path)
		u.logEvent(eventlog.UploadFailed, t, t.LastError)
	case ctx.Err() != nil:
		// Stopped, not failed: the ticket stays ready with its identity.
	default:
		slog.Error("upload failed", "id", t.RecorderID, "s3_key", t.Key, "error", err)
		u.logEvent(eventlog.UploadFailed, t, err.Error()) // Before counting it: the first failure is retry 0.
		t.Attempts++
		t.LastError = err.Error()
		t.NextAttempt = u.clock().Add(uploadBackoff(t.Attempts))
		u.save(t)
	}
}

// uploadBackoff doubles from a minute to maxUploadBackoff; the shift is capped
// before it can overflow on uploads that keep failing.
func uploadBackoff(attempts int) time.Duration {
	return min(maxUploadBackoff, time.Minute<<min(max(attempts-1, 0), 10))
}

// cleanup removes the spool copy of a delivered S3-only recording, then the
// ticket. A failure leaves the ticket delivered, so it is retried.
func (u *uploader) cleanup(t *ticket) {
	err := error(nil)
	if t.DeleteAfterUpload {
		if err = os.Remove(t.Path); errors.Is(err, os.ErrNotExist) { // #nosec G703 -- The recorder-reserved output of the ticket.
			err = nil
		}
	}
	if err == nil {
		err = u.outbox.remove(t)
	}
	if err != nil {
		slog.Warn("cannot clean up a delivered recording; retrying later", "path", t.Path, "error", err)
		t.NextAttempt = u.clock().Add(uploadBackoff(1))
		u.save(t)
	}
}

// save persists t's new state. When that fails the state still applies in
// memory, so a backoff holds and the worker moves on to other recordings; the
// next successful write or a restart reconciles the disk.
func (u *uploader) save(t *ticket) {
	if err := u.outbox.update(t); err != nil {
		slog.Warn("cannot persist upload state", "id", t.ID, "state", t.State, "error", err)
		u.outbox.publish(t)
	}
}

var errRecordingLost = errors.New("recording file is gone and not in S3")

// upload sends t's recording with If-None-Match: * so an existing object is
// never replaced. When the key is taken it moves on to -2, -3, ... from the
// base key; each key is persisted before it is written. The ticket ID is the
// upload identity, so a commit whose response was lost is recognized.
func (u *uploader) upload(ctx context.Context, t *ticket) error {
	dest, err := u.outbox.loadDestination(t.Dest)
	if err != nil {
		return err
	}
	client := createS3Client(&S3Config{
		Endpoint: dest.Endpoint, Bucket: dest.Bucket,
		AccessKeyID: dest.AccessKeyID, SecretAccessKey: dest.SecretAccessKey,
	})
	head := func(ctx context.Context) (bool, error) {
		out, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(dest.Bucket), Key: aws.String(t.Key)})
		if err != nil {
			return false, err
		}
		return out.Metadata["zwfm-upload-id"] == t.ID, nil
	}

	file, info, err := openRecording(t.Path)
	if errors.Is(err, os.ErrNotExist) {
		return verifyMissing(head(ctx))
	}
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			slog.Warn("failed to close file after upload", "path", t.Path, "error", err)
		}
	}()
	size := info.Size()
	if size == 0 {
		return errRecordingLost
	}

	tm := transfermanager.New(client, func(o *transfermanager.Options) {
		o.Concurrency = 5
		o.PartSizeBytes = 8 * 1024 * 1024
		// Abort a multipart upload on a fresh context: a stop cancels ctx,
		// and an unaborted upload keeps its parts stored on S3.
		o.FailTimeout = 5 * time.Second
		o.ObjectProgressListeners.Register(&uploadProgressListener{
			id: t.RecorderID, filename: filepath.Base(t.Path), fileSize: size,
		})
	})
	// A generous timeout: 2 minutes per 50MB, at least 10 minutes.
	timeoutMinutes := max(10, int(size/(50*1024*1024))*2+2)
	ctx, cancel := context.WithTimeoutCause(ctx, time.Duration(timeoutMinutes)*time.Minute, errors.New("s3 upload timeout"))
	defer cancel()
	slog.Info("starting upload", "id", t.RecorderID, "file", filepath.Base(t.Path),
		"size_mb", size/(1024*1024), "timeout_min", timeoutMinutes)

	for suffix := nextKeySuffix(t); ; suffix++ {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("rewind recording for upload: %w", err)
		}
		_, err = tm.UploadObject(ctx, &transfermanager.UploadObjectInput{
			Bucket:        aws.String(dest.Bucket),
			Key:           aws.String(t.Key),
			Body:          file,
			ContentLength: aws.Int64(size),
			ContentType:   aws.String(types.Codec(t.Codec).ContentType()),
			IfNoneMatch:   aws.String("*"),
			Metadata:      map[string]string{"zwfm-upload-id": t.ID},
		})
		if err == nil {
			return nil
		}
		// CompleteMultipartUpload can be committed even if an SDK retry returns NoSuchUpload.
		ours, headErr := head(ctx)
		if headErr == nil && ours {
			return nil
		}
		if !isS3KeyConflict(err) {
			return err
		}
		if headErr != nil {
			return fmt.Errorf("verify conflicting S3 object: %w", headErr)
		}
		if suffix > maxS3KeySuffix {
			return fmt.Errorf("no free S3 key for %s after %d attempts: %w", t.BaseKey, maxS3KeySuffix, err)
		}
		previous := t.Key
		t.Key = withNumericSuffix(t.BaseKey, suffix)
		if err := u.outbox.update(t); err != nil {
			// Never upload under a key a restart would not know.
			t.Key = previous
			return fmt.Errorf("persist upload key: %w", err)
		}
		slog.Warn("S3 key taken, using suffixed key", "id", t.RecorderID, "taken_key", previous, "new_key", t.Key)
	}
}

// verifyMissing decides on a recording whose file is gone from the HEAD of its
// key: delivered before a crash when the object carries its identity, lost
// when there is no such object.
func verifyMissing(ours bool, headErr error) error {
	switch {
	case headErr == nil && ours:
		return nil
	case headErr == nil || isS3NotFound(headErr):
		return errRecordingLost
	default:
		return fmt.Errorf("verify missing recording in S3: %w", headErr)
	}
}

// nextKeySuffix is the suffix after t's current key: -2 after the base key,
// -4 after -3. Suffixing always starts from the base key, never -2-2.
func nextKeySuffix(t *ticket) int {
	for candidate := 2; candidate <= maxS3KeySuffix; candidate++ {
		if t.Key == withNumericSuffix(t.BaseKey, candidate) {
			return candidate + 1
		}
	}
	return 2
}

// openRecording opens a regular recording file without following links.
func openRecording(path string) (*os.File, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("recording %s is not a regular file", path)
	}
	file, err := os.Open(path) //nolint:gosec // Path comes from an outbox ticket.
	if err != nil {
		return nil, nil, fmt.Errorf("open recording for upload: %w", err)
	}
	return file, info, nil
}

// isS3KeyConflict reports whether a conditional upload failed because the key
// is taken or another conditional write to it is in progress. The code also
// covers CompleteMultipartUpload, which reports errors in an HTTP 200 body;
// the status covers a 412 whose body has no parsable code.
func isS3KeyConflict(err error) bool {
	var apiErr interface{ ErrorCode() string }
	if errors.As(err, &apiErr) {
		if code := apiErr.ErrorCode(); code == "PreconditionFailed" || code == "ConditionalRequestConflict" {
			return true
		}
	}
	var responseErr interface{ HTTPStatusCode() int }
	return errors.As(err, &responseErr) && responseErr.HTTPStatusCode() == http.StatusPreconditionFailed
}

func isS3NotFound(err error) bool {
	var responseErr interface{ HTTPStatusCode() int }
	return errors.As(err, &responseErr) && responseErr.HTTPStatusCode() == http.StatusNotFound
}

// checkOverdue reports when the set of overdue recordings becomes non-empty
// or empty again.
func (u *uploader) checkOverdue() {
	u.overdueMu.Lock()
	defer u.overdueMu.Unlock()
	state := overdueState(u.outbox.snapshot(), u.clock())
	u.mu.Lock()
	changed := state.Overdue != u.overdueOpen
	u.overdueOpen = state.Overdue
	cb := u.onOverdue
	u.mu.Unlock()
	if !changed {
		return
	}
	eventType, message := eventlog.UploadOverdueCleared, "Recording uploads delivered"
	if state.Overdue {
		eventType, message = eventlog.UploadOverdue, "Recording uploads overdue"
		slog.Warn(message, "count", state.Count, "oldest", state.Oldest, "error", state.LastError)
	} else {
		slog.Info(message)
	}
	if u.eventLogger != nil {
		if err := u.eventLogger.Log(&eventlog.Event{Type: eventType, Message: message, Details: &eventlog.UploadOverdueDetails{
			Count: state.Count, Oldest: state.Oldest, Recorders: state.Recorders, Error: state.LastError,
		}}); err != nil {
			slog.Warn("failed to log upload overdue event", "error", err)
		}
	}
	if cb != nil {
		cb(state)
	}
}

func overdueState(tickets []ticket, now time.Time) UploadsOverdue {
	var state UploadsOverdue
	for i := range tickets {
		t := &tickets[i]
		if t.State != ticketReady || now.Sub(t.ReadyAt) < uploadOverdueAfter {
			continue
		}
		state.Count++
		if state.Oldest.IsZero() || t.ReadyAt.Before(state.Oldest) {
			state.Oldest, state.LastError = t.ReadyAt, t.LastError
		}
		if !slices.Contains(state.Recorders, t.RecorderName) {
			state.Recorders = append(state.Recorders, t.RecorderName)
		}
	}
	slices.Sort(state.Recorders)
	state.Overdue = state.Count > 0
	return state
}

func (u *uploader) logEvent(eventType eventlog.EventType, t *ticket, errMsg string) {
	if u.eventLogger == nil {
		return
	}
	mode := types.StorageBoth
	if t.DeleteAfterUpload {
		mode = types.StorageS3
	}
	if err := u.eventLogger.LogRecorder(eventType, &eventlog.RecorderDetails{
		RecorderName: t.RecorderName,
		Codec:        t.Codec,
		StorageMode:  string(mode),
		Filename:     filepath.Base(t.Path),
		S3Key:        t.Key,
		Error:        errMsg,
		RetryCount:   t.Attempts,
	}); err != nil {
		slog.Warn("failed to log upload event", "type", eventType, "error", err)
	}
}

// uploadProgressListener logs upload progress. Multipart parts report
// concurrently, so mu guards lastLoggedPct.
type uploadProgressListener struct {
	id            string
	filename      string
	fileSize      int64
	mu            sync.Mutex
	lastLoggedPct int
}

var _ transfermanager.ObjectBytesTransferredListener = (*uploadProgressListener)(nil)

func (l *uploadProgressListener) OnObjectBytesTransferred(
	_ context.Context, e *transfermanager.ObjectBytesTransferredEvent,
) {
	if l.fileSize == 0 {
		return
	}
	pct := int(e.BytesTransferred * 100 / l.fileSize)
	l.mu.Lock()
	defer l.mu.Unlock()
	if pct >= l.lastLoggedPct+25 {
		l.lastLoggedPct = pct
		slog.Debug("upload progress", "id", l.id, "file", l.filename, "progress", pct)
	}
}
