// Package audit implements §12.7: the JSONL audit log of every write PayCLI
// performs.
//
// Two records are written per operation — one before the call and one after —
// so an interrupted destructive operation still leaves a trace. Reads are never
// logged. No credential, JWT, Authorization header or document body ever
// reaches this file: every URL goes through redact.URL, every free-text field
// through redact.Text, and `where` through redact.JSON.
package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

const (
	// FileName is the log's name inside the config directory.
	FileName = "audit.log"
	// FilePerm is §4.1's mode for audit.log.
	FilePerm fs.FileMode = 0o600
	// DefaultMaxBytes is the §12.7 rotation threshold.
	DefaultMaxBytes int64 = 10 << 20
	// DefaultGenerations is how many rotated files are kept (audit.log.1 … .3).
	DefaultGenerations = 3
	// MaxIDs caps Event.IDs; the overflow is reported by IDsTruncated.
	MaxIDs = 200
	// WarnCode is the warning emitted when the POST-write record fails.
	WarnCode = "audit_write_failed"
)

// Phase distinguishes the record written before the call from the one written
// after it.
type Phase string

const (
	// PhasePre is written before the request leaves PayCLI.
	PhasePre Phase = "pre"
	// PhasePost is written after the response (or failure) is known.
	PhasePost Phase = "post"
)

// Event is one audit record (§12.7).
//
// Phase and IDsTruncated are not in §12.7's struct listing: Phase is required
// because a pre-record and a failed post-record are otherwise byte-identical,
// and §12.7's own comment on IDs ("capped at 200 + IDsTruncated") names the
// second one.
type Event struct {
	Time         time.Time       `json:"time"`
	Phase        Phase           `json:"phase"`
	Command      string          `json:"command"`
	Profile      string          `json:"profile"`
	BaseURL      string          `json:"base_url"`
	Collection   string          `json:"collection,omitempty"`
	Global       string          `json:"global,omitempty"`
	Action       string          `json:"action"`
	Method       string          `json:"method"`
	Path         string          `json:"path"`
	Where        json.RawMessage `json:"where,omitempty"`
	IDs          []string        `json:"ids,omitempty"`
	IDsTruncated bool            `json:"ids_truncated,omitempty"`
	Affected     int             `json:"affected"`
	Failed       int             `json:"failed,omitempty"`
	DryRun       bool            `json:"dry_run,omitempty"`
	Status       int             `json:"http_status"`
	OK           bool            `json:"ok"`
	Err          string          `json:"error,omitempty"`
	DurationMS   int64           `json:"duration_ms"`
	RequestID    string          `json:"request_id"`
}

// Options configures a Logger.
type Options struct {
	// Path is the audit log's absolute path. Empty disables the logger, which
	// keeps a mis-resolved config directory from failing every write.
	Path string
	// Disabled is --no-audit / PAY_NO_AUDIT=1.
	Disabled bool
	// MaxBytes overrides DefaultMaxBytes.
	MaxBytes int64
	// Generations overrides DefaultGenerations.
	Generations int
	// Now is the injected clock (§3.1: only app.go may call time.Now).
	Now func() time.Time
	// Secrets are resolved credential literals to scrub from every field, for
	// the case where one reached a string through a path §5.3's key-name rules
	// cannot see.
	Secrets []string
}

// Logger appends audit records.
type Logger struct {
	path        string
	disabled    bool
	maxBytes    int64
	generations int
	now         func() time.Time
	scrubber    redact.Scrubber
}

// New builds a Logger. It never returns an error: an unusable path surfaces at
// write time, where the §12.7 pre/post policy can be applied to it.
func New(opts Options) *Logger {
	l := &Logger{
		path:        opts.Path,
		disabled:    opts.Disabled || opts.Path == "",
		maxBytes:    opts.MaxBytes,
		generations: opts.Generations,
		now:         opts.Now,
	}
	if l.maxBytes <= 0 {
		l.maxBytes = DefaultMaxBytes
	}
	if l.generations <= 0 {
		l.generations = DefaultGenerations
	}
	if l.now == nil {
		// A zero clock is better than a wrong one: the caller must inject.
		l.now = func() time.Time { return time.Time{} }
	}
	if len(opts.Secrets) > 0 {
		l.scrubber = redact.Scrubber{Literals: append([]string(nil), opts.Secrets...)}
	}
	return l
}

// Path is the log's location, for `pay doctor` and `pay audit tail`.
func (l *Logger) Path() string { return l.path }

// Enabled reports whether records are being written.
func (l *Logger) Enabled() bool { return l != nil && !l.disabled }

// Pre writes the before-the-call record. A failure here ABORTS the operation
// (§12.7): the user must opt out of auditing deliberately, never by accident.
func (l *Logger) Pre(e Event) error {
	if !l.Enabled() {
		return nil
	}
	e.Phase = PhasePre
	e.OK = false
	if err := l.write(e); err != nil {
		return apierr.Wrap(err, apierr.CodeAuditWriteFailed,
			"could not write the audit record to %s: %v", l.path, err).
			WithHint("pass --no-audit to proceed without a trace, or fix the permissions on %s", filepath.Dir(l.path))
	}
	return nil
}

// Post writes the after-the-call record. A failure here is a WARNING only
// (§12.7): the mutation already happened, and failing the command would report
// a false negative on a committed write. The returned warning is nil on
// success.
func (l *Logger) Post(e Event) *output.Warning {
	if !l.Enabled() {
		return nil
	}
	e.Phase = PhasePost
	if err := l.write(e); err != nil {
		return &output.Warning{
			Code: WarnCode,
			Message: fmt.Sprintf(
				"the write succeeded but its audit record could not be written to %s: %v", l.path, err),
			Hint: "the operation is committed; fix the audit log's permissions or pass --no-audit to stop trying.",
		}
	}
	return nil
}

// Writable reports whether the audit log can be appended to, for `pay doctor`.
// It creates the directory and an empty log if necessary, which is exactly what
// the first real write would do.
func (l *Logger) Writable() error {
	if !l.Enabled() {
		return nil
	}
	f, err := l.open()
	if err != nil {
		return err
	}
	return f.Close()
}

// write appends one JSON line, rotating first when the line would push the file
// past MaxBytes.
//
// This is the one durable write in PayCLI that does not go through
// fsatomic.Write: an append-only log cannot be rewritten in full per record
// without turning an N-record session into O(N^2) bytes. A single O_APPEND
// write of one complete line is atomic against concurrent `pay` processes on
// every platform PayCLI targets, and rotation itself uses os.Rename.
func (l *Logger) write(e Event) error {
	line, err := l.encode(e)
	if err != nil {
		return err
	}
	if err := l.rotateIfNeeded(int64(len(line))); err != nil {
		return err
	}
	f, err := l.open()
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil && !ignorableSyncErr(err) {
		f.Close()
		return err
	}
	return f.Close()
}

// open creates the directory and the log with §4.1's modes.
func (l *Logger) open() (*os.File, error) {
	if dir := filepath.Dir(l.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, fsatomic.DirPerm); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, FilePerm)
}

// encode redacts the event and renders it as one JSONL line.
func (l *Logger) encode(e Event) ([]byte, error) {
	b, err := json.Marshal(l.sanitize(e))
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// sanitize applies §5.3 to every field that could carry a credential.
func (l *Logger) sanitize(e Event) Event {
	if e.Time.IsZero() {
		e.Time = l.now()
	}
	e.Time = e.Time.UTC()
	e.BaseURL = l.text(redact.URL(e.BaseURL))
	e.Path = l.text(redact.URL(e.Path))
	e.Command = l.text(e.Command)
	e.Profile = l.text(e.Profile)
	e.Collection = l.text(e.Collection)
	e.Global = l.text(e.Global)
	e.Action = l.text(e.Action)
	e.RequestID = l.text(e.RequestID)
	e.Err = l.text(redact.Text(e.Err))
	if len(e.Where) > 0 {
		e.Where = json.RawMessage(l.json(redact.JSON(e.Where).Data))
	}
	if len(e.IDs) > MaxIDs {
		e.IDs = append([]string(nil), e.IDs[:MaxIDs]...)
		e.IDsTruncated = true
	}
	for i, id := range e.IDs {
		e.IDs[i] = l.text(id)
	}
	return e
}

func (l *Logger) text(s string) string {
	if s == "" {
		return s
	}
	return l.scrubber.Text(s)
}

func (l *Logger) json(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	return l.scrubber.JSON(b).Data
}

// ignorableSyncErr tolerates a Sync on a file system that does not implement
// it (some container overlays, network mounts); the record is still written.
func ignorableSyncErr(err error) bool {
	return errors.Is(err, fs.ErrInvalid) || errors.Is(err, errUnsupported)
}

var errUnsupported = errors.ErrUnsupported

// TailOptions filters `pay audit tail`.
type TailOptions struct {
	// N is the maximum number of events returned, most recent last. 0 means
	// DefaultTail.
	N int
	// Since drops events older than this instant. Zero means no bound.
	Since time.Time
	// Action, Command, Profile and Collection are exact-match filters; empty
	// means "any".
	Action     string
	Command    string
	Profile    string
	Collection string
	// Phase filters pre/post records; empty means both.
	Phase Phase
	// IncludeRotated also reads audit.log.1 … .N.
	IncludeRotated bool
}

// DefaultTail is `pay audit tail`'s default -n.
const DefaultTail = 20

// Tail returns the most recent matching events in chronological order.
// Unparseable lines are skipped rather than failing the read: a truncated final
// line after a crash must not make the whole log unreadable.
func (l *Logger) Tail(opts TailOptions) ([]Event, error) {
	if opts.N <= 0 {
		opts.N = DefaultTail
	}
	files := []string{l.path}
	if opts.IncludeRotated {
		// Oldest generation first so the result stays chronological.
		for i := l.generations; i >= 1; i-- {
			files = append([]string{rotatedName(l.path, i)}, files...)
		}
	}

	var out []Event
	for _, name := range files {
		events, err := readEvents(name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, e := range events {
			if matches(e, opts) {
				out = append(out, e)
			}
		}
	}
	if len(out) > opts.N {
		out = out[len(out)-opts.N:]
	}
	return out, nil
}

func matches(e Event, opts TailOptions) bool {
	if opts.Action != "" && !strings.EqualFold(e.Action, opts.Action) {
		return false
	}
	if opts.Command != "" && !strings.EqualFold(e.Command, opts.Command) {
		return false
	}
	if opts.Profile != "" && e.Profile != opts.Profile {
		return false
	}
	if opts.Collection != "" && e.Collection != opts.Collection {
		return false
	}
	if opts.Phase != "" && e.Phase != opts.Phase {
		return false
	}
	if !opts.Since.IsZero() && e.Time.Before(opts.Since) {
		return false
	}
	return true
}

func readEvents(name string) ([]Event, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return out, err
	}
	return out, nil
}
