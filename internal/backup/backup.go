// Package backup implements §12.8: the automatic pre-write backup of every
// document a write is about to change or remove.
//
// A backup is one JSON file per document per write:
//
//	<dir>/<profile>/<collection>/<id>/<UTC-timestamp>--<command>.json
//	<dir>/<profile>/_globals/<slug>/<UTC-timestamp>--<command>.json
//
// holding a Record — the document exactly as the server returned it
// (depth=0, draft=true where drafts exist) plus the facts needed to put it
// back: profile, redacted base URL, collection or global, id, locale and the
// command that was about to overwrite it.
//
// This package owns the on-disk format only. It performs no HTTP and never
// imports internal/cli: the command layer reads the documents and hands them
// here, which keeps the format testable without a server and lets every write
// path share one writer. Every file goes through fsatomic.Write (§3.1) with
// mode 0600 inside 0700 directories, because a document body is exactly as
// sensitive as the CMS it came from.
package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
)

const (
	// FormatVersion is Record.PayBackup. A reader refuses any other value
	// rather than guessing at a future layout.
	FormatVersion = 1
	// FilePerm is the mode of every backup file.
	FilePerm fs.FileMode = 0o600
	// GlobalsDir is the directory globals live under, beside the collection
	// directories. The leading underscore cannot collide with a Payload
	// collection slug that PayCLI would ever address.
	GlobalsDir = "_globals"
	// TimeLayout is the UTC timestamp at the front of every file name:
	// lexically sortable, millisecond resolution and free of ':' so the same
	// name is legal on Windows.
	TimeLayout = "20060102T150405.000Z"
	// Ext is the file extension.
	Ext = ".json"
	// sep separates the timestamp from the command in a file name.
	sep = "--"
)

// Record is one backup file.
type Record struct {
	// PayBackup is the format marker and version; always FormatVersion.
	PayBackup int `json:"pay_backup"`
	// Time is when the document was read, UTC.
	Time time.Time `json:"time"`
	// Profile and BaseURL say where the document lives. BaseURL is redacted
	// (§5.3) before it is written: it is echoed into every file.
	Profile string `json:"profile"`
	BaseURL string `json:"base_url"`
	// Exactly one of Collection and Global is set.
	Collection string `json:"collection,omitempty"`
	Global     string `json:"global,omitempty"`
	// ID is the document id exactly as the server returned it (a JSON number
	// stays a number). Absent for a global.
	ID any `json:"id,omitempty"`
	// Command is the write that was about to run ("update", "delete", …).
	Command string `json:"command"`
	// RequestID is the X-Request-Id of the read that captured Doc, so the
	// capture can be found in the server log.
	RequestID string `json:"request_id"`
	// Draft is true when Doc was read with draft=true (the collection has
	// drafts): Doc is then the newest version, which may be an unpublished
	// draft on top of a published document.
	Draft bool `json:"draft"`
	// Trash is true when Doc was read with trash=true.
	Trash bool `json:"trash,omitempty"`
	// Locale and FallbackLocale are the locale parameters the capture used,
	// which are the ones the write was about to use. A restore sends them
	// back so it writes the same locale it read.
	Locale         string `json:"locale,omitempty"`
	FallbackLocale string `json:"fallback_locale,omitempty"`
	// RedactedPaths lists the values §5.3 masked before the file was written
	// (an apiKey on a users document, anything named *token* or *secret*).
	// A restore never writes those paths back.
	RedactedPaths []string `json:"redacted_paths,omitempty"`
	// CLIVersion is the PayCLI build that wrote the file.
	CLIVersion string `json:"cli_version,omitempty"`
	// Doc is the document.
	Doc map[string]any `json:"doc"`
	// PublishedDoc is the document's main row — its live, published state —
	// captured only when it differs from Doc because a newer draft sits on
	// top of a published document (Doc's _status is "draft", the main row's
	// "published"). That published state is exactly what a non-draft write
	// (update without --draft, publish, apply, upsert, sync) overwrites, so a
	// restore writes it back first and then Doc as the draft on top.
	PublishedDoc map[string]any `json:"published_doc,omitempty"`
}

// Kind reports "global" or "collection".
func (r *Record) Kind() string {
	if r.Global != "" {
		return "global"
	}
	return "collection"
}

// Slug is the collection or global slug.
func (r *Record) Slug() string {
	if r.Global != "" {
		return r.Global
	}
	return r.Collection
}

// IDString is the id as text, "" for a global.
func (r *Record) IDString() string {
	if r == nil || r.ID == nil {
		return ""
	}
	return fmt.Sprint(r.ID)
}

// Ref names the document a backup belongs to, which is what decides its
// directory.
type Ref struct {
	Profile    string
	Collection string
	Global     string
	ID         string
}

// Entry is one backup file as `pay backups list` reports it. Everything is
// derived from the path, so listing thousands of backups reads no file.
type Entry struct {
	Path       string    `json:"path"`
	Profile    string    `json:"profile"`
	Collection string    `json:"collection,omitempty"`
	Global     string    `json:"global,omitempty"`
	ID         string    `json:"id,omitempty"`
	Command    string    `json:"command"`
	Time       time.Time `json:"time"`
	Bytes      int64     `json:"bytes"`
}

// Store is a backup directory.
type Store struct {
	// Root is the configured backup_dir (absolute). Profiles are
	// sub-directories of it.
	Root string
}

// New returns a store rooted at dir.
func New(dir string) *Store { return &Store{Root: dir} }

// ProfileDir is the directory one profile's backups live in.
func (s *Store) ProfileDir(profile string) string {
	return filepath.Join(s.Root, Component(profile))
}

// Dir is the directory one document's backups live in.
func (s *Store) Dir(ref Ref) string {
	if ref.Global != "" {
		return filepath.Join(s.ProfileDir(ref.Profile), GlobalsDir, Component(ref.Global))
	}
	return filepath.Join(s.ProfileDir(ref.Profile), Component(ref.Collection), Component(ref.ID))
}

// Plan is the path a backup of ref taken at t by command would be written to.
// It is what `--dry-run` reports as would_backup; Write may add a ".N" suffix
// when that exact name is already taken.
func (s *Store) Plan(ref Ref, command string, t time.Time) string {
	return filepath.Join(s.Dir(ref), FileName(t, command))
}

// FileName is "<UTC-timestamp>--<command>.json".
func FileName(t time.Time, command string) string {
	return t.UTC().Format(TimeLayout) + sep + CommandSlug(command) + Ext
}

// CommandSlug turns "globals update" into "globals-update".
func CommandSlug(command string) string {
	c := strings.ToLower(strings.TrimSpace(command))
	c = strings.Join(strings.Fields(c), "-")
	if c == "" {
		c = "write"
	}
	return Component(c)
}

// Component makes one path element safe on every platform: anything outside
// [A-Za-z0-9._-] becomes '_', and the two names a path element can never be
// ("." and "..") are prefixed. Payload slugs and numeric, ObjectID or UUID
// ids pass through unchanged.
func Component(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "_"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "." || out == ".." {
		out = "_" + out
	}
	return out
}

// Write stores rec and returns the file's path. The name is unique: a second
// backup of the same document in the same millisecond gets a ".2" suffix
// rather than replacing the first.
func (s *Store) Write(rec *Record) (string, error) {
	if s == nil || s.Root == "" {
		return "", errors.New("backup: no backup directory is configured")
	}
	if rec == nil || rec.Doc == nil {
		return "", errors.New("backup: nothing to write")
	}
	rec.PayBackup = FormatVersion
	rec.Time = rec.Time.UTC()
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("backup: encode: %w", err)
	}
	data = append(data, '\n')

	ref := Ref{Profile: rec.Profile, Collection: rec.Collection, Global: rec.Global, ID: rec.IDString()}
	path := s.Plan(ref, rec.Command, rec.Time)
	path = unique(path)
	if err := fsatomic.Write(path, data, FilePerm); err != nil {
		return "", err
	}
	return path, nil
}

// unique returns path, or path with a ".N" inserted before the extension when
// a file of that name already exists. Only "it exists" moves on to the next
// name: any other answer (a parent that is a file, a permission error) is
// left for the write itself to report, instead of probing forever.
func unique(path string) string {
	if !exists(path) {
		return path
	}
	base := strings.TrimSuffix(path, Ext)
	for n := 2; n < maxUniqueTries; n++ {
		candidate := base + "." + strconv.Itoa(n) + Ext
		if !exists(candidate) {
			return candidate
		}
	}
	return path
}

// maxUniqueTries bounds unique(); a thousand backups of one document in one
// millisecond is not a real workload.
const maxUniqueTries = 1000

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Writable reports whether a backup could be written for profile right now.
// It creates the profile directory — which the first real backup would do
// anyway — and a probe file that it removes again.
func (s *Store) Writable(profile string) error {
	if s == nil || s.Root == "" {
		return errors.New("backup: no backup directory is configured")
	}
	dir := s.ProfileDir(profile)
	if err := os.MkdirAll(dir, fsatomic.DirPerm); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".pay-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// Load reads one backup file. Anything that is not a PayCLI backup of a
// version this build understands is refused by name.
func Load(path string) (*Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, apierr.New(apierr.CodeFileMissing, "no backup file at %s", path).
				WithHint("`pay backups list` prints the path of every backup")
		}
		return nil, apierr.Wrap(err, apierr.CodeInternal, "cannot read %s: %v", path, err)
	}
	return Parse(data, path)
}

// Parse decodes a backup file's bytes. Numbers stay json.Number so an id or a
// numeric field round-trips exactly.
func Parse(data []byte, path string) (*Record, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var rec Record
	if err := dec.Decode(&rec); err != nil {
		return nil, apierr.New(apierr.CodeBadRequestBody, "%s is not valid JSON: %v", path, err).
			WithHint("a PayCLI backup is a JSON object with \"pay_backup\": 1; `pay backups list` prints real ones")
	}
	if rec.PayBackup != FormatVersion {
		return nil, apierr.New(apierr.CodeBadRequestBody,
			"%s is not a PayCLI backup (pay_backup = %d, this build reads %d)", path, rec.PayBackup, FormatVersion).
			WithHint("`pay backups list` prints the path of every backup")
	}
	if rec.Doc == nil || (rec.Collection == "" && rec.Global == "") {
		return nil, apierr.New(apierr.CodeBadRequestBody,
			"%s is a PayCLI backup with no document or no collection/global", path)
	}
	return &rec, nil
}

// Filter narrows List and Prune. Empty fields match everything.
type Filter struct {
	// Profile limits the walk to one profile's directory. Empty walks every
	// profile under the root.
	Profile    string
	Collection string
	Global     string
	ID         string
	Command    string
	// Since keeps entries at or after this instant; Before keeps entries
	// strictly before it. Zero means unbounded.
	Since  time.Time
	Before time.Time
}

func (f Filter) match(e Entry) bool {
	if f.Collection != "" && e.Collection != Component(f.Collection) {
		return false
	}
	if f.Global != "" && e.Global != Component(f.Global) {
		return false
	}
	if f.ID != "" && e.ID != Component(f.ID) {
		return false
	}
	if f.Command != "" && e.Command != CommandSlug(f.Command) {
		return false
	}
	if !f.Since.IsZero() && e.Time.Before(f.Since) {
		return false
	}
	if !f.Before.IsZero() && !e.Time.Before(f.Before) {
		return false
	}
	return true
}

// List returns every backup matching f, newest first. A root that does not
// exist yet is an empty list, not an error: no write has needed it so far.
func (s *Store) List(f Filter) ([]Entry, error) {
	if s == nil || s.Root == "" {
		return nil, nil
	}
	start := s.Root
	if f.Profile != "" {
		start = s.ProfileDir(f.Profile)
	}
	var out []Entry
	err := filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), Ext) {
			return nil
		}
		e, ok := s.parseEntry(path)
		if !ok || !f.match(e) {
			return nil
		}
		if info, err := d.Info(); err == nil {
			e.Bytes = info.Size()
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Time.Equal(out[j].Time) {
			return out[i].Time.After(out[j].Time)
		}
		return out[i].Path > out[j].Path
	})
	return out, nil
}

// parseEntry recovers an Entry from a path below the root. A file that does
// not have the layout's shape (a stray note, a half-written temp file) is
// skipped, never reported as a backup.
func (s *Store) parseEntry(path string) (Entry, bool) {
	rel, err := filepath.Rel(s.Root, path)
	if err != nil {
		return Entry{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 4 {
		return Entry{}, false
	}
	t, command, ok := ParseFileName(parts[3])
	if !ok {
		return Entry{}, false
	}
	e := Entry{Path: path, Profile: parts[0], Command: command, Time: t}
	if parts[1] == GlobalsDir {
		e.Global = parts[2]
	} else {
		e.Collection, e.ID = parts[1], parts[2]
	}
	return e, true
}

// ParseFileName splits "<timestamp>--<command>[.N].json".
func ParseFileName(name string) (time.Time, string, bool) {
	if !strings.HasSuffix(name, Ext) || strings.HasPrefix(name, ".") {
		return time.Time{}, "", false
	}
	stem := strings.TrimSuffix(name, Ext)
	ts, command, ok := strings.Cut(stem, sep)
	if !ok || command == "" {
		return time.Time{}, "", false
	}
	t, err := time.Parse(TimeLayout, ts)
	if err != nil {
		return time.Time{}, "", false
	}
	if i := strings.LastIndexByte(command, '.'); i > 0 {
		if _, err := strconv.Atoi(command[i+1:]); err == nil {
			command = command[:i]
		}
	}
	return t.UTC(), command, true
}

// Prune removes every backup matching f (normally Filter.Before) and then any
// directory the removal left empty. With dryRun it only reports what it
// would remove. It returns the removed (or would-be removed) entries.
func (s *Store) Prune(f Filter, dryRun bool) ([]Entry, error) {
	entries, err := s.List(f)
	if err != nil {
		return nil, err
	}
	if dryRun {
		return entries, nil
	}
	removed := make([]Entry, 0, len(entries))
	dirs := map[string]bool{}
	for _, e := range entries {
		if err := os.Remove(e.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return removed, err
		}
		removed = append(removed, e)
		dirs[filepath.Dir(e.Path)] = true
	}
	s.removeEmptyDirs(dirs)
	return removed, nil
}

// removeEmptyDirs deletes each directory, and its parents up to (not
// including) the root, while they are empty. os.Remove refuses a non-empty
// directory, which is the whole safety argument.
func (s *Store) removeEmptyDirs(dirs map[string]bool) {
	root := filepath.Clean(s.Root)
	list := make([]string, 0, len(dirs))
	for d := range dirs {
		list = append(list, d)
	}
	// Deepest first, so a parent is tried after its children.
	sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	for _, d := range list {
		for dir := filepath.Clean(d); dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)); dir = filepath.Dir(dir) {
			if err := os.Remove(dir); err != nil {
				break
			}
		}
	}
}
