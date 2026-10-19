package decodermonitor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/oszuidwest/zwfm-encoder/internal/util"
)

const maxRecords = 512

// Bookmark keeps the opaque journal cursor and the canonical entry fingerprint.
// The overlap entry, not a comparison of opaque cursor strings, checks continuity.
type Bookmark struct {
	Cursor string `json:"cursor"`
	Key    string `json:"key"`
}

// Record preserves the scoped journal entry's identity and original message.
type Record struct {
	Cursor  string `json:"cursor"`
	Key     string `json:"key"`
	TimeUS  int64  `json:"time_us"`
	BootID  string `json:"boot_id"`
	Message string `json:"message"`
}

// PCM is a read-only kernel snapshot, not a measurement of audible programme level.
type PCM struct {
	State    string `json:"state"`
	Owner    int    `json:"owner"`
	Trigger  string `json:"trigger"`
	Hardware uint64 `json:"hardware"`
	Applied  uint64 `json:"applied"`
}

// Probe reports both journal continuity and playback progress independently.
type Probe struct {
	BootID       string   `json:"boot_id"`
	TimeUS       int64    `json:"time_us"`
	Monotonic    float64  `json:"monotonic"`
	Continuity   string   `json:"continuity"`
	CaughtUp     bool     `json:"caught_up"`
	Cursor       string   `json:"cursor"`
	Key          string   `json:"key"`
	Active       string   `json:"active"`
	MainPID      int      `json:"main_pid"`
	PCM          PCM      `json:"pcm"`
	Records      []Record `json:"records"`
	JournalError bool     `json:"journal_error,omitempty"`
}

func (p *Probe) unverified() bool {
	return !p.CaughtUp || p.Continuity == "gap" || p.Active == "unknown" ||
		p.PCM.State == "unavailable" ||
		(p.Active == "active" && p.PCM.State == "RUNNING" && p.PCM.Owner != p.MainPID)
}

// Prober permits deterministic watchdog tests without opening audio devices.
type Prober interface {
	Read(context.Context, Bookmark) (Probe, error)
}

// LocalProbe reads journalctl and procfs without opening a PCM device.
type LocalProbe struct{ PCMStatus string }

// Read only invokes journalctl and systemctl show, never service-control actions.
func (p LocalProbe) Read(ctx context.Context, bookmark Bookmark) (Probe, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var result Probe
	root, err := os.OpenRoot("/proc")
	if err != nil {
		return Probe{}, fmt.Errorf("open procfs: %w", err)
	}
	defer func() { _ = root.Close() }()
	boot, err := readProc(root, "sys/kernel/random/boot_id")
	if err != nil {
		return Probe{}, err
	}
	uptime, err := readProc(root, "uptime")
	if err != nil {
		return Probe{}, err
	}
	fields := strings.Fields(uptime)
	if len(fields) == 0 {
		return Probe{}, fmt.Errorf("missing system uptime")
	}
	result.Monotonic, err = strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return Probe{}, fmt.Errorf("invalid system uptime")
	}
	result.BootID = strings.TrimSpace(boot)
	result.PCM = readPCM(root, p.PCMStatus)
	statusCtx, statusCancel := context.WithTimeout(ctx, 2*time.Second)
	result.Active, result.MainPID, err = serviceStatus(statusCtx)
	statusCancel()
	if err != nil {
		result.Active = "unknown"
	}
	// Journal failures must not blind the independent kernel playback check.
	journalCtx, journalCancel := context.WithTimeout(ctx, 5*time.Second)
	journal, journalErr := readJournal(journalCtx, bookmark)
	if journalErr == nil && journal.Continuity == "gap" {
		journal, journalErr = readJournal(journalCtx, Bookmark{})
		journal.Continuity = "gap"
	}
	journalCancel()
	if journalErr != nil {
		result.Cursor, result.Key = bookmark.Cursor, bookmark.Key
		result.Continuity, result.JournalError = "gap", true
	} else {
		result.Cursor, result.Key, result.Records = journal.Cursor, journal.Key, journal.Records
		result.Continuity, result.CaughtUp = journal.Continuity, journal.CaughtUp
	}
	result.TimeUS = time.Now().UnixMicro()
	return result, nil
}

func readJournal(ctx context.Context, bookmark Bookmark) (Probe, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := []string{"--unit=micrompx.service", "--no-pager", "--quiet", "--all", "--output=json", "--no-tail"}
	if bookmark.Cursor != "" {
		if len(bookmark.Cursor) > 4096 || strings.ContainsRune(bookmark.Cursor, 0) {
			return Probe{}, fmt.Errorf("invalid saved journal cursor")
		}
		args = append(args, "--cursor="+bookmark.Cursor)
	} else {
		args = append(args, "--since=@"+strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10))
	}
	cmd := util.CommandContext(ctx, "/usr/bin/journalctl", args...)
	cmd.WaitDelay = time.Second
	stderr := &boundedBuffer{limit: 8192}
	cmd.Stderr = stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return Probe{}, fmt.Errorf("open journal output: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Probe{}, fmt.Errorf("start journal reader: %w", err)
	}
	result, readErr := parseJournal(pipe, bookmark)
	truncated := readErr == nil && (!result.CaughtUp || result.Continuity == "gap")
	if readErr != nil || truncated {
		cancel()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return Probe{}, readErr
	}
	if !truncated && waitErr != nil {
		return Probe{}, fmt.Errorf("journal reader failed: %w", waitErr)
	}
	if stderr.Len() != 0 {
		// Includes permission/corruption warnings even if journalctl exits zero.
		return Probe{}, fmt.Errorf("journal reader reported incomplete access or a diagnostic")
	}
	return result, nil
}

func parseJournal(input io.Reader, bookmark Bookmark) (Probe, error) {
	result := Probe{Continuity: "initial", Cursor: bookmark.Cursor, Key: bookmark.Key}
	scanner := bufio.NewScanner(io.LimitReader(input, maxDocument+1))
	scanner.Buffer(make([]byte, 4096), 128<<10)
	first, size := true, 0
	for scanner.Scan() {
		line := scanner.Bytes()
		size += len(line) + 1
		if size > maxDocument || len(result.Records) >= maxRecords {
			return result, nil
		}
		record, err := decodeRecord(line)
		if err != nil {
			return Probe{}, err
		}
		if first && bookmark.Cursor != "" {
			first = false
			if bookmark.Key == "" || record.Key != bookmark.Key {
				result.Continuity = "gap"
				return result, nil
			}
			result.Continuity = "ok"
			continue
		}
		first = false
		result.Records = append(result.Records, record)
		result.Cursor, result.Key = record.Cursor, record.Key
	}
	if err := scanner.Err(); err != nil {
		return Probe{}, fmt.Errorf("read journal output: %w", err)
	}
	if first && bookmark.Cursor != "" {
		result.Continuity = "gap"
		return result, nil
	}
	result.CaughtUp = true
	return result, nil
}

func decodeRecord(line []byte) (Record, error) {
	var fields map[string]any
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		return Record{}, fmt.Errorf("invalid journal JSON")
	}
	text := func(key string) string { value, _ := fields[key].(string); return value }
	record := Record{Cursor: text("__CURSOR"), BootID: text("_BOOT_ID"), Message: text("MESSAGE")}
	var err error
	record.TimeUS, err = strconv.ParseInt(text("__REALTIME_TIMESTAMP"), 10, 64)
	if err != nil || record.TimeUS <= 0 || record.Cursor == "" || record.BootID == "" {
		return Record{}, fmt.Errorf("journal entry lacks identity metadata")
	}
	if _, ok := fields["MESSAGE"].(string); !ok {
		return Record{}, fmt.Errorf("journal message is not textual")
	}
	delete(fields, "__CURSOR")
	canonical, err := json.Marshal(fields)
	if err != nil {
		return Record{}, fmt.Errorf("canonicalize journal entry: %w", err)
	}
	record.Key = fmt.Sprintf("%x", sha256.Sum256(canonical))
	return record, nil
}

func serviceStatus(ctx context.Context) (active string, pid int, err error) {
	cmd := util.CommandContext(ctx, "/usr/bin/systemctl", "show", "--no-pager", "micrompx.service", "-p", "ActiveState", "-p", "MainPID")
	output := &boundedBuffer{limit: 8192}
	cmd.Stdout, cmd.WaitDelay = output, time.Second
	if err := cmd.Run(); err != nil {
		return "", 0, fmt.Errorf("read decoder service status: %w", err)
	}
	values := parseFields(output.String(), "=")
	pid, err = strconv.Atoi(values["MainPID"])
	if err != nil || values["ActiveState"] == "" {
		return "", 0, fmt.Errorf("incomplete decoder service status")
	}
	return values["ActiveState"], pid, nil
}

func readProc(root *os.Root, name string) (string, error) {
	f, err := root.Open(name)
	if err != nil {
		return "", fmt.Errorf("read kernel status: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(data) > 16384 {
		return "", fmt.Errorf("invalid kernel status")
	}
	return string(data), nil
}

func readPCM(root *os.Root, path string) PCM {
	if !pcmPath.MatchString(path) {
		return PCM{State: "unavailable"}
	}
	text, err := readProc(root, strings.TrimPrefix(path, "/proc/"))
	if err != nil {
		return PCM{State: "unavailable"}
	}
	if strings.TrimSpace(text) == "closed" {
		return PCM{State: "closed"}
	}
	fields := parseFields(text, ":")
	owner, err := strconv.Atoi(fields["owner_pid"])
	if err != nil || owner <= 0 {
		return PCM{State: "unavailable"}
	}
	process, err := readProc(root, fmt.Sprintf("%d/status", owner))
	if err != nil {
		return PCM{State: "unavailable"}
	}
	pid, err := strconv.Atoi(parseFields(process, ":")["Tgid"])
	if err != nil {
		return PCM{State: "unavailable"}
	}
	hardware, err := strconv.ParseUint(fields["hw_ptr"], 10, 64)
	if err != nil {
		return PCM{State: "unavailable"}
	}
	applied, err := strconv.ParseUint(fields["appl_ptr"], 10, 64)
	if err != nil {
		return PCM{State: "unavailable"}
	}
	if fields["state"] == "" || fields["trigger_time"] == "" {
		return PCM{State: "unavailable"}
	}
	return PCM{State: fields["state"], Owner: pid, Trigger: fields["trigger_time"], Hardware: hardware, Applied: applied}
}

func parseFields(text, separator string) map[string]string {
	values := make(map[string]string)
	for line := range strings.SplitSeq(text, "\n") {
		if key, value, ok := strings.Cut(line, separator); ok {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return values
}

type boundedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *boundedBuffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *boundedBuffer) Len() int       { return b.buffer.Len() }
func (b *boundedBuffer) String() string { return b.buffer.String() }

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if len(data) > b.limit-b.Len() {
		return 0, fmt.Errorf("probe output exceeds size limit")
	}
	return b.buffer.Write(data)
}
