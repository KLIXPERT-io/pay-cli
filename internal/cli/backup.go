package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/backup"
	"github.com/KLIXPERT-io/pay-cli/internal/buildinfo"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// ---------------------------------------------------------------------------
// §12.8 automatic pre-write backups — the hook every write path shares
// ---------------------------------------------------------------------------
//
// When a backup directory is configured (--backup-dir, PAY_BACKUP_DIR or
// `backup_dir` in pay.toml / config.toml) every write that changes or removes
// an EXISTING document first reads it (depth=0, draft=true where drafts exist,
// trash=true where trash exists, the write's own locale) and stores it as one
// JSON file per document. Only then is the pre-write audit record written and
// the request sent.
//
// Two ways in, one implementation:
//
//   - writeOp.withBackup(ctx, BackupTarget{…}) — the normal path. writeOp.pre
//     takes the backup before the audit pre-record, so a backup that cannot be
//     taken aborts the write exactly like an audit record that cannot be
//     written; emitDryRun reports would_backup instead of reading anything.
//     Every verb that PATCHes, POSTs over or DELETEs an existing document uses
//     this: update (by id and bulk), apply, publish/unpublish, delete (soft,
//     permanent, bulk, passthrough), restore, versions restore, globals update,
//     upload --replace and backups restore.
//
//   - Deps.BackupBeforeWrite(ctx, command, BackupTarget{…}) — the exported
//     hook for a write that does not go through writeOp, or that performs
//     several requests for one logical change (upsert's update half, sync, the
//     orphan-row two-phase write). It is idempotent per invocation: a document
//     already backed up by this process is not read or written again, so a
//     two-phase write backs up the PRE-image once, never its own phase 1.

// Warning codes raised by the backup hook.
const (
	// WarnBackupSkipped names targeted documents the backup read did not
	// return. The write still runs — it will answer for those ids itself
	// (usually doc_not_found) — but nothing was saved for them.
	WarnBackupSkipped = "backup_skipped"
	// WarnBackupsOff is `pay backups …` run with no backup directory.
	WarnBackupsOff = "backups_disabled"
	// WarnBackupRedacted names the credential values a backup masked: the
	// file does not hold them, so a restore cannot bring them back.
	WarnBackupRedacted = "backup_redacted"
)

// backupReadChunk is how many ids one backup read asks for. It matches the
// bulk-write chunk (§12.3) so a 250-document update costs three reads.
const backupReadChunk = payload.BulkChunk

// BackupTarget names the documents a write is about to change.
//
// Exactly one of Collection and Global is set. IDs are the collection
// document ids; when a writeOp carries the target, nil IDs means "the ids the
// writeOp was given" (withIDs), which is how the bulk verbs hand over the ids
// §12.3 resolved. Params are the WRITE's query parameters: only Locale,
// FallbackLocale and Trash are consulted — the read always uses depth=0, and
// draft=true exactly when the entity has drafts.
type BackupTarget struct {
	Collection *collTarget
	Global     *globalTarget
	IDs        []any
	Params     query.Params
}

// BackupResult is what one backup pass produced.
type BackupResult struct {
	// Paths are the files written (or, for documents this invocation had
	// already backed up, the file written the first time).
	Paths []string
	// Skipped are the ids the backup read did not return.
	Skipped []any
	// Redacted are the values masked in the files, as "<doc>: <path>". They
	// are not in the backup, so a restore cannot bring them back.
	Redacted []string
}

// backupPreview is data.would_backup on a dry run.
type backupPreview struct {
	// Dir is the profile's backup directory.
	Dir string `json:"dir"`
	// Documents is how many files the real run would write.
	Documents int `json:"documents"`
	// Paths are the first safety.SampleLimit planned file names. The
	// timestamp is the dry run's; the real run stamps its own.
	Paths []string `json:"paths"`
	// Truncated says Paths is not every file.
	Truncated bool `json:"truncated"`
}

// backupStore is the configured store, or nil when backups are off for this
// invocation (no directory, or --no-backup / PAY_NO_BACKUP).
func (rt *Runtime) backupStore() *backup.Store {
	if rt == nil || rt.Cfg == nil || rt.Cfg.NoBackup || rt.Cfg.BackupDir == "" {
		return nil
	}
	return backup.New(rt.Cfg.BackupDir)
}

// recordBackups remembers files written by this invocation for meta.backups.
func (rt *Runtime) recordBackups(paths ...string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.backupPaths = append(rt.backupPaths, paths...)
}

// backupsWritten is meta.backups.
func (rt *Runtime) backupsWritten() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.backupPaths) == 0 {
		return nil
	}
	return append([]string(nil), rt.backupPaths...)
}

// seenBackup returns the file this invocation already wrote for key.
func (rt *Runtime) seenBackup(key string) (string, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	p, ok := rt.backupSeen[key]
	return p, ok
}

func (rt *Runtime) markBackup(key, path string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.backupSeen == nil {
		rt.backupSeen = map[string]string{}
	}
	rt.backupSeen[key] = path
}

// keepBackupDoc remembers the pre-image a backup captured.
func (rt *Runtime) keepBackupDoc(key string, doc map[string]any) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.backupDocs == nil {
		rt.backupDocs = map[string]map[string]any{}
	}
	rt.backupDocs[key] = doc
}

// backedUpDoc is the pre-image this invocation's backup captured for one
// document (collection: kind "collection", id; global: kind "global", id ""),
// or nil when none was taken. It is the NEWEST version (the backup reads with
// draft=true where drafts exist).
func (d *Deps) backedUpDoc(kind, slug, id, locale string) map[string]any {
	if d == nil || d.RT == nil {
		return nil
	}
	d.RT.mu.Lock()
	defer d.RT.mu.Unlock()
	return d.RT.backupDocs[backupKey(kind, slug, id, locale)]
}

// BackupsEnabled reports whether a write in this invocation will be backed up.
// A caller that must do extra work only to name the documents (resolve ids
// for the passthrough delete, look up a version's parent) checks this first,
// so a user without backups never pays for it.
func (d *Deps) BackupsEnabled() bool {
	return d != nil && d.RT != nil && d.RT.backupStore() != nil
}

// BackupBeforeWrite is the exported §12.8 hook: it backs up every document bt
// names and returns the files. With backups off it does nothing and returns
// an empty result. Any failure — the read or the file — is backup_failed
// (exit 1) with the --no-backup hint, and the caller MUST NOT send its write.
func (d *Deps) BackupBeforeWrite(ctx context.Context, command string, bt BackupTarget) (*BackupResult, error) {
	res := &BackupResult{}
	if !d.BackupsEnabled() {
		return res, nil
	}
	if d.cfg().DryRun {
		// A dry run writes nothing, backups included; emitDryRun reports
		// would_backup instead.
		return res, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	store := d.RT.backupStore()
	client, err := d.requireClient()
	if err != nil {
		return nil, backupFailed(err, store.Root, "no client to read the document with")
	}

	switch {
	case bt.Global != nil:
		if err := d.backupGlobal(ctx, client, store, command, bt, res); err != nil {
			return nil, err
		}
	case bt.Collection != nil:
		if err := d.backupDocs(ctx, client, store, command, bt, res); err != nil {
			return nil, err
		}
	}
	if len(res.Redacted) > 0 {
		d.RT.Warn(output.Warning{
			Code: WarnBackupRedacted,
			Message: fmt.Sprintf("the backup masked %d credential value(s) before writing the file; they are not in the "+
				"backup, so `pay backups restore` cannot bring them back", len(res.Redacted)),
			Paths: capPaths(res.Redacted),
			Hint:  "they keep their current value on a restore; pass --no-redact to back up every value verbatim",
		})
	}
	if len(res.Skipped) > 0 {
		d.RT.Warn(output.Warning{
			Code: WarnBackupSkipped,
			Message: fmt.Sprintf("the backup read did not return %d of the targeted document(s) (%s), so nothing was backed up for them",
				len(res.Skipped), joinIDs(res.Skipped)),
			Hint: "the server answered doc_not_found for them, so the write will fail for them too; `pay backups list` shows what was saved",
		})
	}
	return res, nil
}

// backupKey identifies one document for the per-invocation dedupe.
func backupKey(kind, slug, id, locale string) string {
	return kind + "\x00" + slug + "\x00" + id + "\x00" + locale
}

// backupReadParams is the read a backup takes: the document as the server
// holds it, not as the write's echo projection would show it.
func backupReadParams(bt BackupTarget, drafts, trash *bool) query.Params {
	p := query.Params{Depth: query.IntPtr(0)}
	if drafts != nil && *drafts {
		p.Draft = query.BoolPtr(true)
	}
	if trash != nil && *trash {
		// trash=true returns trashed AND live documents (verified on Payload
		// 3.87), so it is the one read that finds every id the write can.
		p.Trash = query.BoolPtr(true)
	}
	p.Locale = bt.Params.Locale
	p.FallbackLocale = bt.Params.FallbackLocale
	return p
}

func (d *Deps) backupDocs(ctx context.Context, client *payload.Client, store *backup.Store, command string,
	bt BackupTarget, res *BackupResult) error {
	t := bt.Collection
	flags := t.flags()
	base := backupReadParams(bt, flags.Drafts, flags.Trash)

	// Dedupe first: ids this invocation already backed up are not read again.
	var todo []any
	for _, id := range bt.IDs {
		key := backupKey("collection", t.Slug, fmt.Sprint(id), base.Locale)
		if path, ok := d.RT.seenBackup(key); ok {
			res.Paths = append(res.Paths, path)
			continue
		}
		todo = append(todo, id)
	}

	for start := 0; start < len(todo); start += backupReadChunk {
		end := min(start+backupReadChunk, len(todo))
		chunk := todo[start:end]
		p := base.Clone()
		p.Where = safety.WhereIDsIn(chunk)
		p.Limit = query.IntPtr(backupReadChunk)
		index := newDocsByID()
		requestIDs := map[string]string{}
		err := client.FindPages(ctx, t.Slug, p, len(chunk), func(l *payload.ListResult) error {
			rid := ""
			if l.HTTP != nil {
				rid = l.HTTP.RequestID
			}
			for _, doc := range l.Docs {
				index.add(doc)
				requestIDs[doc.IDString()] = rid
			}
			return nil
		}, payload.WithKnownRoute())
		if err != nil {
			return backupFailed(err, store.Root, fmt.Sprintf("reading %d %s document(s) to back up", len(chunk), t.Slug))
		}
		// An id the batched read did not return is re-read on its own before
		// it is given up on: "skipped" must mean the server said the
		// document does not exist, never that PayCLI could not pair a
		// returned document with the id it was asked for.
		for _, id := range chunk {
			if _, _, ok := index.lookup(id); ok {
				continue
			}
			doc, resp, err := client.Get(ctx, t.Slug, fmt.Sprint(id), base, payload.WithKnownRoute())
			switch {
			case err != nil && apierr.HasCode(err, apierr.CodeDocNotFound):
				continue
			case err != nil:
				return backupFailed(err, store.Root, fmt.Sprintf("reading %s %v to back up", t.Slug, id))
			}
			if doc == nil {
				continue
			}
			if doc.IDString() == "" {
				doc["id"] = id
			}
			index.add(doc)
			rid := ""
			if resp != nil {
				rid = resp.RequestID
			}
			requestIDs[doc.IDString()] = rid
			if _, _, ok := index.lookup(id); !ok {
				// The server answered for this id with a document PayCLI
				// cannot pair with it: back it up under the id as sent
				// rather than write without a backup.
				index.exact[fmt.Sprint(id)] = doc
				requestIDs[fmt.Sprint(id)] = rid
			}
		}
		found := index.exact
		published, err := d.backupPublishedRows(ctx, client, t, base, found)
		if err != nil {
			return backupFailed(err, store.Root, fmt.Sprintf("reading the published state of %d %s document(s) to back up", len(chunk), t.Slug))
		}
		var written []string
		for _, id := range chunk {
			doc, key, ok := index.lookup(id)
			if !ok {
				res.Skipped = append(res.Skipped, id)
				continue
			}
			if path, done := d.RT.seenBackup(backupKey("collection", t.Slug, key, base.Locale)); done {
				// `108` and `0108` in one call: the same document, one file.
				if !slices.Contains(written, path) && !slices.Contains(res.Paths, path) {
					res.Paths = append(res.Paths, path)
				}
				continue
			}
			rec := d.newBackupRecord(command, base, requestIDs[key], doc, published[key], isTrue(flags.Auth))
			rec.Collection = t.Slug
			rec.ID = doc.ID()
			if rec.ID == nil {
				rec.ID = id
			}
			path, err := store.Write(rec)
			if err != nil {
				d.RT.recordBackups(written...)
				return backupFailed(err, store.Root, fmt.Sprintf("writing the backup of %s %s", t.Slug, key))
			}
			d.RT.markBackup(backupKey("collection", t.Slug, key, base.Locale), path)
			d.RT.keepBackupDoc(backupKey("collection", t.Slug, key, base.Locale), map[string]any(doc))
			res.noteRedacted(t.Slug+" "+key, rec.RedactedPaths)
			written = append(written, path)
		}
		d.RT.recordBackups(written...)
		res.Paths = append(res.Paths, written...)
	}
	return nil
}

func (d *Deps) backupGlobal(ctx context.Context, client *payload.Client, store *backup.Store, command string,
	bt BackupTarget, res *BackupResult) error {
	g := bt.Global
	flags := g.flags()
	p := backupReadParams(bt, flags.Drafts, nil)
	key := backupKey("global", g.Slug, "", p.Locale)
	if path, ok := d.RT.seenBackup(key); ok {
		res.Paths = append(res.Paths, path)
		return nil
	}
	doc, resp, err := client.GlobalGet(ctx, g.Slug, p, payload.WithKnownRoute())
	if err != nil {
		return backupFailed(err, store.Root, "reading the global "+g.Slug+" to back it up")
	}
	rid := ""
	if resp != nil {
		rid = resp.RequestID
	}
	// A newer draft on top of a published global: capture the published
	// state too (see backupPublishedRows).
	var published map[string]any
	if p.Draft != nil && *p.Draft && docStatus(doc) == "draft" {
		mp := p.Clone()
		mp.Draft = nil
		main, _, err := client.GlobalGet(ctx, g.Slug, mp, payload.WithKnownRoute())
		if err != nil {
			return backupFailed(err, store.Root, "reading the published state of the global "+g.Slug+" to back it up")
		}
		if docStatus(main) == "published" {
			published = map[string]any(main)
		}
	}
	rec := d.newBackupRecord(command, p, rid, doc, published, false)
	rec.Global = g.Slug
	path, err := store.Write(rec)
	if err != nil {
		return backupFailed(err, store.Root, "writing the backup of the global "+g.Slug)
	}
	d.RT.markBackup(key, path)
	d.RT.keepBackupDoc(key, map[string]any(doc))
	res.noteRedacted("global "+g.Slug, rec.RedactedPaths)
	d.RT.recordBackups(path)
	res.Paths = append(res.Paths, path)
	return nil
}

// newBackupRecord fills everything but the collection/global and id.
//
// published is the document's main row when it differs from doc (a newer
// draft on top of a published document), else nil.
//
// Unless --no-redact was given, credentials are masked before the document
// touches the disk (redact.StoredDoc): a document read back at depth 0 can
// carry a populated user's apiKey, and a backup directory is not a credential
// store. Only credentials: every masked value is one a restore cannot bring
// back, so content that merely looks secret-ish (designTokens, a JWT-shaped
// embed token) is kept. authRoot says the collection itself is an auth
// collection, whose top-level hash/salt/password/sessions are credentials.
// The masked paths are recorded so a restore leaves them alone instead of
// writing the mask back.
func (d *Deps) newBackupRecord(command string, p query.Params, requestID string, doc payload.Doc,
	published map[string]any, authRoot bool) *backup.Record {
	cfg := d.cfg()
	rec := &backup.Record{
		Time:           d.now().UTC(),
		Profile:        cfg.Profile,
		BaseURL:        redact.URL(cfg.BaseURL),
		Command:        command,
		RequestID:      requestID,
		Draft:          p.Draft != nil && *p.Draft,
		Trash:          p.Trash != nil && *p.Trash,
		Locale:         p.Locale,
		FallbackLocale: p.FallbackLocale,
		CLIVersion:     buildinfo.Version(),
		Doc:            map[string]any(doc),
		PublishedDoc:   published,
	}
	if cfg.Redact {
		scrub := redact.Scrubber{Literals: d.RT.Secrets()}
		seen := map[string]bool{}
		mask := func(v map[string]any) map[string]any {
			out, paths := scrub.StoredDoc(v, authRoot)
			for _, p := range paths {
				if !seen[p] {
					seen[p] = true
					rec.RedactedPaths = append(rec.RedactedPaths, p)
				}
			}
			if m, ok := out.(map[string]any); ok && len(paths) > 0 {
				return m
			}
			return v
		}
		rec.Doc = mask(rec.Doc)
		if rec.PublishedDoc != nil {
			rec.PublishedDoc = mask(rec.PublishedDoc)
		}
		sortStrings(rec.RedactedPaths)
	}
	return rec
}

// backupPublishedRows reads the main row of every document in found whose
// newest version is a draft, and returns those whose main row is published:
// a published document with a newer draft on top. Only collections with
// drafts read anything, and only when the chunk holds such a draft, so the
// common case — no pending drafts — costs no extra request.
//
// Why: the backup read uses draft=true and so captures the NEWEST version.
// A non-draft write over a published document with a pending draft (update
// without --draft, publish, apply, upsert, sync) overwrites the main row —
// the live, published state — and Payload's PATCH base is the draft, so the
// write can even take the page offline. Without this second read that
// published state would be in no backup file.
func (d *Deps) backupPublishedRows(ctx context.Context, client *payload.Client, t *collTarget, base query.Params,
	found map[string]payload.Doc) (map[string]map[string]any, error) {
	if base.Draft == nil || !*base.Draft {
		return nil, nil
	}
	var drafts []any
	for key, doc := range found {
		if docStatus(doc) == "draft" {
			id := doc.ID()
			if id == nil {
				id = key
			}
			drafts = append(drafts, id)
		}
	}
	if len(drafts) == 0 {
		return nil, nil
	}
	sortAnyIDs(drafts)
	p := base.Clone()
	p.Draft = nil
	p.Where = safety.WhereIDsIn(drafts)
	p.Limit = query.IntPtr(backupReadChunk)
	out := map[string]map[string]any{}
	err := client.FindPages(ctx, t.Slug, p, len(drafts), func(l *payload.ListResult) error {
		for _, doc := range l.Docs {
			if docStatus(doc) == "published" {
				out[doc.IDString()] = map[string]any(doc)
			}
		}
		return nil
	}, payload.WithKnownRoute())
	if err != nil {
		return nil, err
	}
	return out, nil
}

// docStatus is a document's _status ("" without drafts).
func docStatus(doc map[string]any) string {
	s, _ := doc["_status"].(string)
	return s
}

// sortAnyIDs orders ids by their text so the read is deterministic.
func sortAnyIDs(ids []any) {
	sort.Slice(ids, func(i, j int) bool { return fmt.Sprint(ids[i]) < fmt.Sprint(ids[j]) })
}

func isTrue(b *bool) bool { return b != nil && *b }

// noteRedacted remembers which paths of which document the backup masked, for
// the write's backup_redacted warning.
func (r *BackupResult) noteRedacted(doc string, paths []string) {
	for _, p := range paths {
		r.Redacted = append(r.Redacted, doc+": "+p)
	}
}

// backupFailed is §12.8's abort: exit 1, nothing sent, and the one flag that
// proceeds deliberately without a backup.
func backupFailed(cause error, root, doing string) error {
	msg := cause.Error()
	if e, ok := apierr.As(cause); ok {
		msg = e.Message
	}
	return apierr.Wrap(cause, apierr.CodeBackupFailed,
		"the pre-write backup failed while %s: %s; nothing was written", doing, msg).
		WithHint("fix the cause above — `pay doctor` checks the backup directory %s — "+
			"or pass --no-backup to write without a backup, deliberately", root)
}

// BackupPreview is would_backup for a dry run: the files the real run would
// write, without reading or writing anything. nil when backups are off.
func (d *Deps) BackupPreview(command string, bt BackupTarget) *backupPreview {
	if !d.BackupsEnabled() {
		return nil
	}
	store := d.RT.backupStore()
	cfg := d.cfg()
	now := d.now()
	prev := &backupPreview{Dir: store.ProfileDir(cfg.Profile), Paths: []string{}}
	switch {
	case bt.Global != nil:
		prev.Documents = 1
		prev.Paths = append(prev.Paths, store.Plan(backup.Ref{Profile: cfg.Profile, Global: bt.Global.Slug}, command, now))
	case bt.Collection != nil:
		prev.Documents = len(bt.IDs)
		for i, id := range bt.IDs {
			if i >= safety.SampleLimit {
				prev.Truncated = true
				break
			}
			prev.Paths = append(prev.Paths, store.Plan(backup.Ref{
				Profile: cfg.Profile, Collection: bt.Collection.Slug, ID: fmt.Sprint(id),
			}, command, now))
		}
	default:
		return nil
	}
	return prev
}

// ---------------------------------------------------------------------------
// writeOp integration
// ---------------------------------------------------------------------------

// withBackup makes the write back up bt before its pre-write audit record.
// ctx is kept for the read; pre() has no context parameter of its own.
func (w *writeOp) withBackup(ctx context.Context, bt BackupTarget) *writeOp {
	w.bk = &bt
	w.bkCtx = ctx
	return w
}

// withRestoredFrom names the backup file a `pay backups restore` writes back,
// for the audit records.
func (w *writeOp) withRestoredFrom(file string) *writeOp {
	w.restoredFrom = file
	return w
}

// backupTarget resolves the target at the moment it is needed: a bulk verb
// sets its ids with withIDs after withBackup, and those are the ones to save.
func (w *writeOp) backupTarget() (BackupTarget, bool) {
	if w == nil || w.bk == nil {
		return BackupTarget{}, false
	}
	bt := *w.bk
	if bt.Collection != nil && bt.IDs == nil {
		bt.IDs = w.ids
	}
	return bt, true
}

// takeBackup runs the hook for a writeOp. It is called from pre().
func (w *writeOp) takeBackup() error {
	bt, ok := w.backupTarget()
	if !ok || w.d == nil {
		return nil
	}
	if bt.Collection != nil && len(bt.IDs) == 0 {
		return nil
	}
	res, err := w.d.BackupBeforeWrite(w.bkCtx, w.op.Command, bt)
	if err != nil {
		return err
	}
	w.backups = res.Paths
	return nil
}

// wouldBackup is the dry-run preview for a writeOp, or nil.
func (w *writeOp) wouldBackup() *backupPreview {
	bt, ok := w.backupTarget()
	if !ok || w.d == nil {
		return nil
	}
	if bt.Collection != nil && len(bt.IDs) == 0 {
		return nil
	}
	return w.d.BackupPreview(w.op.Command, bt)
}

// resolveBackupFile finds FILE for `pay backups show|restore`: as given
// (relative to the working directory), then relative to the backup
// directory, so the paths `pay backups list` prints and the shorter
// "<profile>/<collection>/<id>/<file>" form both work.
func (rt *Runtime) resolveBackupFile(arg string) string {
	if filepath.IsAbs(arg) {
		return arg
	}
	candidates := []string{}
	if rt.App.WorkDir != "" {
		candidates = append(candidates, filepath.Join(rt.App.WorkDir, arg))
	} else {
		candidates = append(candidates, arg)
	}
	if rt.Cfg != nil && rt.Cfg.BackupDir != "" {
		candidates = append(candidates, filepath.Join(rt.Cfg.BackupDir, arg))
		// A path copied out of `pay backups list` for one profile directory.
		candidates = append(candidates, filepath.Join(rt.Cfg.BackupDir, backup.Component(rt.Cfg.Profile), arg))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); !errors.Is(err, fs.ErrNotExist) {
			return c
		}
	}
	return candidates[0]
}

// backupsDirOrWarn is the directory `pay backups list|prune` read, with the
// warning an agent needs when there is none.
func (rt *Runtime) backupsDirOrWarn() string {
	if rt.Cfg != nil && rt.Cfg.BackupDir != "" {
		return rt.Cfg.BackupDir
	}
	rt.Warn(output.Warning{
		Code:    WarnBackupsOff,
		Message: "no backup directory is configured, so no write has been backed up",
		Hint:    "set PAY_BACKUP_DIR (or backup_dir in ./pay.toml, or pass --backup-dir) to back up every document before a write changes it",
	})
	return ""
}

// versionRestoreBackupTarget names the document `pay versions restore` is
// about to overwrite. A global is its own target; a collection version is
// looked up once for its `parent`, because the writeOp's ids are VERSION ids.
// It returns nil when backups are off, so the lookup costs nothing then. A
// version that cannot be read is returned as the server's own error
// (version_not_found): the restore would have failed on it anyway.
func versionRestoreBackupTarget(d *Deps, scope versionScope, versionID string, p query.Params,
	readVersion func() (payload.Doc, error)) (*BackupTarget, error) {
	if !d.BackupsEnabled() {
		return nil, nil
	}
	if scope.global != nil {
		return &BackupTarget{Global: scope.global, Params: p}, nil
	}
	ver, err := readVersion()
	if err != nil {
		return nil, err
	}
	parent := ver["parent"]
	if obj, ok := parent.(map[string]any); ok {
		parent = obj["id"]
	}
	if parent == nil {
		return nil, backupFailed(errors.New("the version record carries no parent document id"),
			d.RT.backupStore().Root, "looking up which document version "+versionID+" belongs to")
	}
	return &BackupTarget{Collection: scope.coll, IDs: []any{parent}, Params: p}, nil
}
