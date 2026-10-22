package recording

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The outbox is the delivery queue on disk: one ticket per recording, written
// when its output is reserved, so a crash at any point leaves the recording
// findable. A ticket has one owner at a time: the recorder until the recording
// is finalized, then the uploader. The owner persists a change before it is
// published in the in-memory index.

type ticketState string

const (
	// ticketRecording: the output is reserved and may be receiving audio.
	ticketRecording ticketState = "recording"
	// ticketReady: finalized and waiting for upload.
	ticketReady ticketState = "ready"
	// ticketDelivered: the object is in S3; only local cleanup is left.
	ticketDelivered ticketState = "delivered"
	// ticketFailed: finalization failed; kept for the operator, never uploaded.
	ticketFailed ticketState = "failed"
	// ticketLost: audio that was synced to disk is gone.
	ticketLost ticketState = "lost"
)

// ticket is the persisted delivery state of one recording. Its ID is also the
// upload identity stored in the object's metadata.
type ticket struct {
	ID                string      `json:"id"`
	Seq               uint64      `json:"seq"`
	State             ticketState `json:"state"`
	RecorderID        string      `json:"recorder_id"`
	RecorderName      string      `json:"recorder_name"`
	Path              string      `json:"path"`
	Dest              string      `json:"dest,omitempty"` // Destination ID; empty in local-only mode.
	BaseKey           string      `json:"base_key,omitempty"`
	Key               string      `json:"key,omitempty"` // Last key tried; a taken key moves to -2, -3, ...
	Codec             string      `json:"codec,omitempty"`
	DeleteAfterUpload bool        `json:"delete_after_upload,omitempty"`
	SyncedBytes       int64       `json:"synced_bytes,omitempty"`
	ReadyAt           time.Time   `json:"ready_at,omitzero"`
	Attempts          int         `json:"attempts,omitempty"`
	NextAttempt       time.Time   `json:"next_attempt,omitzero"`
	LastError         string      `json:"last_error,omitempty"`
}

func (s ticketState) valid() bool {
	switch s {
	case ticketRecording, ticketReady, ticketDelivered, ticketFailed, ticketLost:
		return true
	default:
		return false
	}
}

// destinationID scopes credentials to one recorder and one bucket, so recorders
// that share a bucket with different credentials stay apart, and a credential
// change of a recorder reaches its undelivered recordings.
func destinationID(recorderID, endpoint, bucket string) string {
	sum := sha256.Sum256([]byte(recorderID + "\x00" + endpoint + "\x00" + bucket))
	return hex.EncodeToString(sum[:8])
}

// failedMarkerSuffix marks a recording as failed when its ticket could not be
// updated, so a restart does not deliver it.
const failedMarkerSuffix = ".failed"

type outbox struct {
	dir string

	mu         sync.Mutex
	tickets    map[string]ticket // By ID.
	paths      map[string]string // Path to ticket ID.
	destMu     sync.Mutex        // Serializes destination writes.
	unreadable int               // Tickets a read error kept from loading; counted as lost.
	wake       chan struct{}
}

const ticketSuffix = ".json"

func (o *outbox) destinationDir() string { return filepath.Join(o.dir, "destinations") }

// openOutbox loads the outbox in spoolDir and applies crash recovery.
func openOutbox(spoolDir string, logInterrupted func(t *ticket)) (*outbox, error) {
	o := &outbox{
		dir:     filepath.Join(spoolDir, "outbox"),
		tickets: make(map[string]ticket),
		paths:   make(map[string]string),
		wake:    make(chan struct{}, 1),
	}
	if err := mkdirSynced(o.destinationDir(), 0o750); err != nil {
		return nil, fmt.Errorf("create outbox: %w", err)
	}
	// Also when an earlier start created them but could not sync them.
	if err := errors.Join(syncDir(spoolDir), syncDir(o.dir)); err != nil {
		return nil, fmt.Errorf("sync outbox: %w", err)
	}
	loaded, err := o.load()
	if err != nil {
		return nil, err
	}
	referenced := make(map[string]bool)
	for i := range loaded {
		t := &loaded[i]
		if !o.recover(t, logInterrupted) {
			continue
		}
		o.tickets[t.ID] = *t
		o.paths[t.Path] = t.ID
		if t.Dest != "" {
			referenced[t.Dest] = true
		}
	}
	if o.unreadable == 0 {
		// An unreadable ticket may need any destination: keep them all.
		o.removeUnreferencedDestinations(referenced)
	}
	return o, nil
}

// load reads the newest valid generation of every ticket and removes the
// others: older generations and a newer one torn by a crash.
func (o *outbox) load() ([]ticket, error) {
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return nil, fmt.Errorf("read outbox: %w", err)
	}
	byID := make(map[string][]generation)
	for _, entry := range entries {
		id, seq, ok := parseTicketName(entry.Name())
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		byID[id] = append(byID[id], generation{seq, entry.Name()})
	}
	var loaded []ticket
	for id, gens := range byID {
		best, readFailed := o.newestGeneration(id, gens)
		if readFailed {
			// Leave every generation in place for the next start.
			o.unreadable++
			continue
		}
		for _, g := range gens {
			if best == nil || g.seq != best.Seq {
				if err := os.Remove(filepath.Join(o.dir, g.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
					slog.Warn("cannot remove stale ticket generation", "file", g.name, "error", err)
				}
			}
		}
		if best != nil {
			loaded = append(loaded, *best)
		} else {
			slog.Warn("removed unreadable outbox ticket", "id", id)
		}
	}
	return loaded, syncDir(o.dir)
}

type generation struct {
	seq  uint64
	name string
}

// newestGeneration returns the newest valid generation of ticket id. A
// generation that cannot be read, rather than parsed, may be the newest, so
// readFailed tells the caller to leave the ticket alone.
func (o *outbox) newestGeneration(id string, gens []generation) (best *ticket, readFailed bool) {
	for _, g := range gens {
		data, err := os.ReadFile(filepath.Join(o.dir, g.name)) //nolint:gosec // An outbox directory entry.
		if err != nil {
			slog.Error("cannot read outbox ticket", "file", g.name, "error", err)
			readFailed = true
			continue
		}
		var t ticket
		if json.Unmarshal(data, &t) == nil && t.ID == id && t.Seq == g.seq && t.State.valid() && t.Path != "" &&
			(best == nil || t.Seq > best.Seq) {
			best = &t
		}
	}
	return best, readFailed
}

func parseTicketName(name string) (id string, seq uint64, ok bool) {
	base, found := strings.CutSuffix(name, ticketSuffix)
	if !found {
		return "", 0, false
	}
	id, seqText, found := strings.Cut(base, ".")
	if !found || id == "" {
		return "", 0, false
	}
	seq, err := strconv.ParseUint(seqText, 10, 64)
	return id, seq, err == nil
}

// recover applies crash recovery to a loaded ticket and reports whether it is
// kept. Only a ticket still in ticketRecording was interrupted.
func (o *outbox) recover(t *ticket, logInterrupted func(*ticket)) bool {
	if t.State == ticketFailed || t.State == ticketLost {
		// The operator handles these by removing the file; a restart then
		// clears the ticket.
		if _, err := os.Lstat(t.Path); errors.Is(err, os.ErrNotExist) {
			return !o.dropRecovered(t)
		}
		return true
	}
	if t.State != ticketRecording && t.State != ticketReady {
		return true
	}
	if _, err := os.Lstat(t.Path + failedMarkerSuffix); err == nil {
		// A failure whose ticket update did not persist.
		o.persistRecovered(t, ticketFailed, cmp.Or(t.LastError, "recording failed"))
		return true
	}
	if t.State == ticketReady {
		return true // The uploader verifies a missing file against S3.
	}
	return o.recoverInterrupted(t, logInterrupted)
}

// recoverInterrupted handles a ticket a crash left in ticketRecording.
func (o *outbox) recoverInterrupted(t *ticket, logInterrupted func(*ticket)) bool {
	info, err := os.Lstat(t.Path)
	switch {
	case errors.Is(err, os.ErrNotExist) && t.SyncedBytes == 0:
		return !o.dropRecovered(t)
	case errors.Is(err, os.ErrNotExist):
		o.persistRecovered(t, ticketLost, "recording disappeared after a crash")
		return true
	case err != nil:
		// Unknown, such as an unavailable share: keep it for the next start.
		slog.Warn("cannot check interrupted recording", "path", t.Path, "error", err)
		return true
	case !info.Mode().IsRegular():
		o.persistRecovered(t, ticketFailed, "recording is not a regular file")
		return true
	case info.Size() == 0 && t.SyncedBytes == 0:
		if err := os.Remove(t.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("cannot remove empty interrupted recording", "path", t.Path, "error", err)
			return true
		}
		return !o.dropRecovered(t)
	case info.Size() == 0:
		o.persistRecovered(t, ticketLost, "synced audio is gone after a crash")
		return true
	}
	if err := syncFile(t.Path); err != nil {
		slog.Warn("cannot sync interrupted recording", "path", t.Path, "error", err)
		return true
	}
	if info.Size() < t.SyncedBytes {
		slog.Warn("interrupted recording is shorter than its synced length",
			"path", t.Path, "size", info.Size(), "synced", t.SyncedBytes)
	}
	logInterrupted(t)
	if t.Dest == "" { // Local-only: the file is the recording.
		return !o.dropRecovered(t)
	}
	t.ReadyAt = time.Now()
	o.persistRecovered(t, ticketReady, t.LastError)
	return true
}

func (o *outbox) persistRecovered(t *ticket, state ticketState, reason string) {
	t.State, t.LastError = state, reason
	if err := o.write(t); err != nil {
		slog.Warn("cannot persist recovered ticket", "id", t.ID, "error", err)
	}
}

// dropRecovered removes a ticket during recovery and reports whether it went;
// one that cannot be removed is kept for the next start.
func (o *outbox) dropRecovered(t *ticket) bool {
	if err := o.removeFiles(t); err != nil {
		slog.Warn("cannot remove recovered ticket", "id", t.ID, "error", err)
		return false
	}
	return true
}

// newTicket persists t as a new ticket for a reserved output; it sets its ID.
func (o *outbox) newTicket(t *ticket) error {
	t.ID, t.Seq, t.State = rand.Text(), 0, ticketRecording
	return o.update(t)
}

// update persists a change of a ticket the caller owns, then publishes it.
func (o *outbox) update(t *ticket) error {
	if err := o.write(t); err != nil {
		return err
	}
	o.publish(t)
	return nil
}

// fail moves a ticket to ticketFailed. When that cannot be persisted, a marker
// next to the recording keeps a restart from delivering it.
func (o *outbox) fail(t *ticket, cause error) {
	t.State, t.LastError = ticketFailed, cause.Error()
	if err := o.write(t); err != nil {
		slog.Warn("cannot persist failed recording; marking it next to the file", "path", t.Path, "error", err)
		markErr := writeSynced(t.Path+failedMarkerSuffix, []byte(cause.Error()+"\n"))
		if markErr == nil {
			markErr = syncDir(filepath.Dir(t.Path))
		}
		if markErr != nil {
			slog.Error("cannot mark failed recording", "path", t.Path, "error", markErr)
		}
	}
	o.publish(t)
}

// remove deletes a ticket the caller owns, all its generations included.
func (o *outbox) remove(t *ticket) error {
	if err := o.removeFiles(t); err != nil {
		return err
	}
	o.mu.Lock()
	delete(o.tickets, t.ID)
	if o.paths[t.Path] == t.ID {
		delete(o.paths, t.Path)
	}
	o.mu.Unlock()
	return nil
}

func (o *outbox) removeFiles(t *ticket) error {
	// Older generations go first and the newest last, so a failed removal
	// leaves the newest state in place instead of an older one that a restart
	// would bring back.
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return err
	}
	var newest string
	var newestSeq uint64
	var older []string
	for _, entry := range entries {
		id, seq, ok := parseTicketName(entry.Name())
		switch {
		case !ok || id != t.ID:
		case newest == "" || seq > newestSeq:
			if newest != "" {
				older = append(older, newest)
			}
			newest, newestSeq = entry.Name(), seq
		default:
			older = append(older, entry.Name())
		}
	}
	for _, name := range older {
		if err := os.Remove(filepath.Join(o.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) { // #nosec G703 -- An outbox directory entry.
			return err
		}
	}
	if err := syncDir(o.dir); err != nil {
		return err
	}
	if newest != "" {
		if err := os.Remove(filepath.Join(o.dir, newest)); err != nil && !errors.Is(err, os.ErrNotExist) { // #nosec G703 -- An outbox directory entry.
			return err
		}
		if err := syncDir(o.dir); err != nil {
			return err
		}
	}
	if err := os.Remove(t.Path + failedMarkerSuffix); err != nil && !errors.Is(err, os.ErrNotExist) { // #nosec G703 -- The marker of a recorder-reserved output.
		slog.Warn("cannot remove failed-recording marker", "path", t.Path, "error", err)
	}
	return nil
}

func (o *outbox) publish(t *ticket) {
	o.mu.Lock()
	o.tickets[t.ID] = *t
	o.paths[t.Path] = t.ID
	o.mu.Unlock()
	if t.State == ticketReady || t.State == ticketDelivered {
		o.signal()
	}
}

func (o *outbox) signal() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// write persists t as its next generation and removes the previous one. A new
// generation is a new file, so no step depends on an atomic replace.
func (o *outbox) write(t *ticket) error {
	next := *t
	var name string
	for {
		next.Seq++
		data, err := json.MarshalIndent(&next, "", "  ")
		if err != nil {
			return err
		}
		name = filepath.Join(o.dir, next.ID+"."+strconv.FormatUint(next.Seq, 10)+ticketSuffix)
		// A generation can exist already when an earlier write's directory
		// sync failed: build on top of it.
		if err = writeSynced(name, data); !errors.Is(err, os.ErrExist) {
			if err != nil {
				return fmt.Errorf("write ticket: %w", err)
			}
			break
		}
	}
	if err := syncDir(o.dir); err != nil {
		// The generation exists but may not survive a power cut: the next
		// write builds on it, and the previous one stays as a fallback.
		*t = next
		return fmt.Errorf("sync outbox: %w", err)
	}
	if next.Seq > 1 {
		previous := filepath.Join(o.dir, next.ID+"."+strconv.FormatUint(next.Seq-1, 10)+ticketSuffix)
		if err := os.Remove(previous); err != nil && !errors.Is(err, os.ErrNotExist) { // #nosec G703 -- An outbox-owned generation name.
			slog.Warn("cannot remove previous ticket generation", "file", previous, "error", err)
		}
	}
	*t = next
	return nil
}

// hasPath reports whether a ticket owns path, whether or not its file exists.
func (o *outbox) hasPath(path string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.paths[path]
	return ok
}

// snapshot returns a copy of every ticket.
func (o *outbox) snapshot() []ticket {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]ticket, 0, len(o.tickets))
	for id := range o.tickets {
		out = append(out, o.tickets[id])
	}
	return out
}

// nextDue returns the deliverable ticket that is due first, and how long to
// wait when none is due yet. A delivered ticket only needs its local cleanup.
func (o *outbox) nextDue(now time.Time) (t ticket, due bool, wait time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	wait = time.Hour
	for id := range o.tickets {
		c := o.tickets[id]
		if c.State != ticketReady && c.State != ticketDelivered {
			continue
		}
		if !c.NextAttempt.After(now) {
			if !due || c.ReadyAt.Before(t.ReadyAt) {
				t, due = c, true
			}
			continue
		}
		wait = min(wait, c.NextAttempt.Sub(now))
	}
	if due {
		wait = 0
	}
	return t, due, wait
}

func (o *outbox) destinationPath(id string) string {
	return filepath.Join(o.destinationDir(), id+".json")
}

// saveDestinationLocked must be called with destMu held.
func (o *outbox) saveDestinationLocked(id string, d S3Config) error {
	data, _ := json.Marshal(&d) // Only string fields.
	path := o.destinationPath(id)
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) { //nolint:gosec // Outbox-owned path.
		return nil
	}
	tmp := path + ".tmp"
	if err := writeSynced(tmp, data); err != nil {
		_ = os.Remove(tmp) // #nosec G703 -- The outbox-owned temporary file.
		return err
	}
	if err := os.Rename(tmp, path); err != nil { // #nosec G703 -- Outbox-owned destination files.
		_ = os.Remove(tmp) // #nosec G703 -- The outbox-owned temporary file.
		return err
	}
	return syncDir(o.destinationDir())
}

// ensureDestination stores d only when id has no credentials yet. A
// reservation uses it: its config may be older than an update that already
// stored new credentials.
func (o *outbox) ensureDestination(id string, d S3Config) error {
	o.destMu.Lock()
	defer o.destMu.Unlock()
	if _, err := os.Stat(o.destinationPath(id)); err == nil { // #nosec G703 -- An outbox-owned destination file.
		return nil
	}
	return o.saveDestinationLocked(id, d)
}

func (o *outbox) loadDestination(id string) (S3Config, error) {
	var d S3Config
	data, err := os.ReadFile(o.destinationPath(id)) // #nosec G703 -- An outbox-owned destination file.
	if err != nil {
		return d, fmt.Errorf("read upload destination: %w", err)
	}
	err = json.Unmarshal(data, &d)
	return d, err
}

func (o *outbox) removeUnreferencedDestinations(referenced map[string]bool) {
	entries, err := os.ReadDir(o.destinationDir())
	if err != nil {
		return
	}
	for _, entry := range entries {
		id, ok := strings.CutSuffix(entry.Name(), ".json")
		if ok && referenced[id] {
			continue
		}
		_ = os.Remove(filepath.Join(o.destinationDir(), entry.Name()))
	}
}

// writeSynced creates name exclusively and makes its content durable.
func writeSynced(name string, data []byte) error {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // Callers pass outbox or recording paths.
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	// Sync and close even on failure; preserve the first error.
	err = cmp.Or(err, f.Sync(), f.Close())
	if err != nil {
		_ = os.Remove(name) // #nosec G703 -- The file this call just created.
	}
	return err
}

func syncFile(name string) error {
	f, err := os.OpenFile(name, os.O_WRONLY, 0) //nolint:gosec // Callers pass a recording path.
	if err != nil {
		return err
	}
	err = f.Sync()
	return errors.Join(err, f.Close())
}

// syncDir makes directory entries durable. Windows has no directory fsync; a
// file flushed through its own handle is durable there.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir) //nolint:gosec // Callers pass outbox or recording directories.
	if err != nil {
		return err
	}
	err = f.Sync()
	return errors.Join(err, f.Close())
}

// mkdirSynced creates dir with perm and makes the new entries durable in
// their parents.
func mkdirSynced(dir string, perm os.FileMode) error {
	var created []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil || filepath.Dir(d) == d { // #nosec G703 -- The configured spool directory.
			break
		}
		created = append(created, d)
	}
	if err := os.MkdirAll(dir, perm); err != nil { // #nosec G703 G301 -- The configured spool directory; callers choose perm.
		return err
	}
	for _, d := range created {
		if err := syncDir(filepath.Dir(d)); err != nil {
			return err
		}
	}
	return nil
}
