package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/backup"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/docdiff"
	"github.com/KLIXPERT-io/pay-cli/internal/orphans"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

// `pay backups` is the read-and-recover side of §12.8. The write side is
// backup.go's hook, which every write verb calls before it changes a document.

// Warning codes `pay backups restore` raises.
const (
	// WarnRestoredAsDraft: the backup holds a draft, the live document is
	// published, so the restore saved a new draft instead of unpublishing.
	WarnRestoredAsDraft = "restored_as_draft"
	// WarnRecreatedNewID: --recreate made a new document; the id changed.
	WarnRecreatedNewID = "backup_recreated_new_id"
	// WarnRecreateIgnored: --recreate was passed but the document exists.
	WarnRecreateIgnored = "recreate_not_needed"
	// WarnFieldsNotRestored: fields the restore deliberately did not send.
	WarnFieldsNotRestored = "backup_fields_not_restored"
	// WarnRecreatedInTrash: the backup was of a trashed document, and so is
	// the re-created one.
	WarnRecreatedInTrash = "backup_recreated_in_trash"
	// WarnPublishedNotRestored: the backup holds a published state under a
	// newer draft, and this restore did not put it back.
	WarnPublishedNotRestored = "published_state_not_restored"
	// WarnRestoreTwoSteps: the dry run of a restore that writes the backup's
	// published state and then its draft on top — two versions.
	WarnRestoreTwoSteps = "restore_two_steps"
)

// defaultBackupsListN is `pay backups list`'s default -n.
const defaultBackupsListN = 50

func init() { Register(newBackupsCmd) }

func newBackupsCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "backups",
		GroupID: GroupAdmin,
		Short:   "List, inspect, restore and prune the automatic pre-write backups",
		Long: `With a backup directory configured — PAY_BACKUP_DIR, backup_dir in ./pay.toml or
config.toml, or --backup-dir — every write that changes or removes an existing
document first saves it as one JSON file:

  <dir>/<profile>/<collection>/<id>/<UTC-timestamp>--<command>.json
  <dir>/<profile>/_globals/<slug>/<UTC-timestamp>--<command>.json

Covered: update (by id and bulk), apply, publish, unpublish, delete (trash,
--permanent, bulk), restore, versions restore, globals update, upload --replace
and backups restore itself. Not covered: create, duplicate and pay raw.

A backup that cannot be taken ABORTS the write (backup_failed, exit 1);
--no-backup (or PAY_NO_BACKUP=1) writes without one, deliberately.`,
	}
	cmd.AddCommand(newBackupsListCmd(rt), newBackupsShowCmd(rt), newBackupsRestoreCmd(rt), newBackupsPruneCmd(rt))
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay backups list|show|restore|prune"},
		Output:    OutputSpec{Kind: output.KindOpResult},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitValidation, apierr.ExitConfig},
		Examples: []Example{
			{Why: "turn backups on for every command in this shell", Cmd: "export PAY_BACKUP_DIR=~/pay-backups"},
			{Why: "what was saved before the last writes?", Cmd: "pay backups list"},
			{Why: "every backup of one document", Cmd: "pay backups list --collection pages --id 16"},
			{Why: "read one back", Cmd: "pay backups show <file> --path .doc.title"},
			{Why: "put it back — preview first", Cmd: "pay backups restore <file> --dry-run"},
			{Why: "delete backups older than 30 days", Cmd: "pay backups prune --older-than 30d --yes"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting backups without configuring a directory.",
				Right: "They are off until PAY_BACKUP_DIR, backup_dir or --backup-dir names one. `pay doctor` reports the directory and whether it is writable."},
			{Wrong: "Hand-copying `pay get` output into files before a risky write.",
				Right: "With a backup directory configured, every write does exactly that itself and lists the files in meta.backups."},
			{Wrong: "Treating backup_failed as a server error and retrying.",
				Right: "Nothing was sent. Fix the directory, or re-run with --no-backup if writing without a backup is really what you want."},
		},
		SeeAlso: []string{"pay doctor", "pay audit tail", "pay versions list <collection> --id <id>"},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

type backupsListData struct {
	Dir      string         `json:"dir"`
	Profile  string         `json:"profile"`
	Writing  bool           `json:"writing"`
	Returned int            `json:"returned"`
	Total    int            `json:"total"`
	Backups  []backup.Entry `json:"backups"`
}

func newBackupsListCmd(rt *Runtime) *cobra.Command {
	var (
		n           int
		since       string
		collection  string
		global      string
		id          string
		command     string
		allProfiles bool
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List backup files, newest first",
		Long: `List the backup files of the current profile, newest first. Everything is read
from the file names, so listing never opens a backup.`,
		Args: maxArgs(0, "pay backups list [--collection C] [--id ID] [--since D]"),
		RunE: Handle(rt, "backups list", func(_ context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			if n < 0 {
				return nil, apierr.New(apierr.CodeInvalidArgs, "-n must not be negative")
			}
			f := backup.Filter{Collection: collection, Global: global, ID: id, Command: command}
			if !allProfiles {
				f.Profile = rt.Cfg.Profile
			}
			if since != "" {
				t, err := query.ParseSince(since, rt.Now())
				if err != nil {
					return nil, err
				}
				f.Since = t
			}
			dir := rt.backupsDirOrWarn()
			entries, err := backup.New(dir).List(f)
			if err != nil {
				return nil, apierr.Wrap(err, apierr.CodeInternal, "cannot read the backup directory %s: %v", dir, err).
					WithHint("`pay doctor` checks the backup directory")
			}
			if entries == nil {
				entries = []backup.Entry{}
			}
			data := backupsListData{
				Dir: dir, Profile: rt.Cfg.Profile, Writing: rt.backupStore() != nil,
				Total: len(entries), Backups: entries,
			}
			if allProfiles {
				data.Profile = ""
			}
			if n > 0 && len(entries) > n {
				data.Backups = entries[:n]
			}
			data.Returned = len(data.Backups)
			env := output.New("backups list", output.KindOpResult, jsonValue(data))
			if dir != "" && rt.Cfg.NoBackup {
				env.AddWarning(output.Warning{
					Code:    WarnBackupsOff,
					Message: "--no-backup / PAY_NO_BACKUP is set, so this invocation would not write a backup; existing ones are listed",
					Hint:    "drop --no-backup and unset PAY_NO_BACKUP to back up writes again",
				})
			}
			if len(data.Backups) > 0 {
				env.WithNext(&output.Next{
					Reason: output.ReasonVerifyWrite,
					Cmd:    "pay backups show " + shellQuote(data.Backups[0].Path),
				})
			}
			return env, nil
		}),
	}
	fl := cmd.Flags()
	fl.IntVarP(&n, "n", "n", defaultBackupsListN, "maximum number of backups to return (0 = all)")
	fl.StringVar(&since, "since", "", "only backups newer than this (7d, 24h, 2026-01-31, RFC3339)")
	fl.StringVar(&collection, "collection", "", "only this collection")
	fl.StringVar(&global, "global", "", "only this global")
	fl.StringVar(&id, "id", "", "only this document id")
	fl.StringVar(&command, "command", "", "only backups taken before this command, e.g. delete")
	fl.BoolVar(&allProfiles, "all-profiles", false, "list every profile's backups, not just the current one")

	SetHelp(cmd, &Help{
		Synopsis: []string{
			"pay backups list [-n N] [--since D] [--collection C] [--id ID] [--global G]",
			"                 [--command C] [--all-profiles]",
		},
		Output: OutputSpec{Kind: output.KindOpResult,
			Skeleton: `{"dir":"/home/u/pay-backups","profile":"local","writing":true,"returned":1,"total":1,"backups":[{"path":"…/local/pages/16/20260928T120000.000Z--update.json","profile":"local","collection":"pages","id":"16","command":"update","time":"2026-09-28T12:00:00Z","bytes":2048}]}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitValidation, apierr.ExitConfig},
		Examples: []Example{
			{Why: "the most recent backups of this profile", Cmd: "pay backups list"},
			{Why: "every backup of one page", Cmd: "pay backups list --collection pages --id 16 -n 0"},
			{Why: "what was saved before deletes this week", Cmd: "pay backups list --command delete --since 7d"},
			{Why: "a global's history", Cmd: "pay backups list --global header"},
			{Why: "the newest file's path, for a script", Cmd: "pay backups list -n 1 --path .backups[0].path --output id"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting another profile's backups in the list.",
				Right: "The list is scoped to the current profile; pass --all-profiles to see every one under the directory."},
			{Wrong: "Reading an empty list as \"nothing was ever changed\".",
				Right: "Check .dir and .writing, and warnings[]: with no backup directory configured nothing is backed up at all."},
			{Wrong: "Filtering --command with the envelope name \"globals update\".",
				Right: "Either form works; the file name stores it as globals-update."},
		},
		SeeAlso: []string{"pay backups show <file>", "pay backups restore <file> --dry-run", "pay audit tail"},
	})
	return cmd
}

// ---------------------------------------------------------------------------
// show
// ---------------------------------------------------------------------------

func newBackupsShowCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <file>",
		Short: "Print one backup file: where it came from and the document",
		Args:  exactArgs(1, "pay backups show <file>"),
		RunE: Handle(rt, "backups show", func(_ context.Context, rt *Runtime, args []string) (*output.Envelope, error) {
			path := rt.resolveBackupFile(args[0])
			rec, err := backup.Load(path)
			if err != nil {
				return nil, backupFileError(err, path)
			}
			data, _ := jsonValue(rec).(map[string]any)
			if data == nil {
				data = map[string]any{}
			}
			data["path"] = path
			env := output.New("backups show", output.KindOpResult, data)
			env.WithNext(&output.Next{
				Reason: output.ReasonVerifyWrite,
				Cmd:    "pay backups restore " + shellQuote(path) + " --dry-run",
			})
			return env, nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis: []string{"pay backups show <file>"},
		Args: []ArgSpec{{Name: "file", Required: true, Type: "string",
			Example: "a path from `pay backups list`, absolute or relative to the backup directory"}},
		Output: OutputSpec{Kind: output.KindOpResult,
			Skeleton: `{"path":"…","pay_backup":1,"time":"…","profile":"local","base_url":"http://localhost:3900","collection":"pages","id":16,"command":"update","request_id":"…","draft":true,"doc":{"id":16,"title":"…"}}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitValidation},
		Examples: []Example{
			{Why: "the whole record", Cmd: "pay backups show ~/pay-backups/local/pages/16/20260928T120000.000Z--update.json"},
			{Why: "one field of the saved document", Cmd: "pay backups show <file> --path .doc.title"},
			{Why: "where it came from", Cmd: "pay backups show <file> --path .base_url"},
			{Why: "a path relative to the backup directory", Cmd: "pay backups show local/pages/16/20260928T120000.000Z--update.json"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting the document at the depth you read it with.",
				Right: "Backups are always depth=0 (relationships are ids) and draft=true where drafts exist — the shape a write needs."},
			{Wrong: "Assuming `<redacted>` values are the real ones.",
				Right: "Credentials only are masked before the file is written: apiKey, reset/verification tokens, a user's hash/salt/password/sessions, and the literal API key or preview secret. .redacted_paths lists them, the write warned backup_redacted, and a restore never writes them back. --no-redact keeps them."},
			{Wrong: "Expecting .doc to be what the live page shows.",
				Right: ".doc is the NEWEST version (a pending draft, if any). When a published document had a newer draft, .published_doc holds the published state too, and a restore puts back both."},
			{Wrong: "Editing the file and restoring it.",
				Right: "It works, but it is no longer a backup. Prefer `pay update <collection> <id> --data-file` for a deliberate change."},
		},
		SeeAlso: []string{"pay backups list", "pay backups restore <file> --dry-run"},
	})
	return cmd
}

// backupFileError keeps a missing or foreign file a validation failure with a
// path, never a bare internal error.
func backupFileError(err error, path string) error {
	if e, ok := apierr.As(err); ok && e.Code == apierr.CodeFileMissing {
		return apierr.New(apierr.CodeInvalidArgs, "no backup file at %s", path).
			WithHint("`pay backups list` prints the path of every backup of this profile")
	}
	return err
}

// ---------------------------------------------------------------------------
// restore
// ---------------------------------------------------------------------------

type backupsRestoreFlags struct {
	recreate     bool
	ignoreTarget bool
	asDraft      bool
	publish      bool
	depth        int
	noEchoCheck  bool

	// rec and path are the loaded, target-checked backup.
	rec  *backup.Record
	path string
}

func newBackupsRestoreCmd(rt *Runtime) *cobra.Command {
	f := &backupsRestoreFlags{}
	cmd := &cobra.Command{
		Use:   "restore <file>",
		Short: "Write a backup back: PATCH the document, or --recreate a deleted one",
		Long: `Write a backup file back to the server it came from.

The stored document is sent as the new state of that document (a PATCH; a POST
for a global), minus the fields the server owns: id, createdAt, updatedAt, join
fields, read-only and upload-derived fields, and any value that was redacted
when the backup was written. The current state is itself backed up first.

Drafts: a backup of a published document is restored published. A backup of a
draft is saved as a draft (draft=true, so an incomplete draft restores like it
was saved); over a live published document that is a NEW DRAFT on top of it
(restored_as_draft), never an unpublish. A backup of a published document with
a newer draft on top holds both (doc and published_doc): the restore writes
the published state back first and then the draft on top (two requests, both
audited; data.steps in the dry run), so the page is live again with its old
content and its old draft pending. --draft and --publish override the choice.

A document that was permanently deleted cannot be PATCHed: --recreate creates
it again. Payload assigns a NEW id — relationships that pointed at the old id
are not updated (backup_recreated_new_id).`,
		Args: exactArgs(1, "pay backups restore <file> [--dry-run] [--yes] [--recreate]"),
	}
	data := runData(rt, safety.CmdBackupsRestore, func(ctx context.Context, _ *cobra.Command, d *Deps, _ []string) (*output.Envelope, error) {
		return runBackupsRestore(ctx, d, f)
	})
	// The file and its target are checked BEFORE anything talks to a server:
	// a backup from another profile must be refused as such, not reported as
	// whatever the wrong server happens to answer.
	cmd.RunE = func(c *cobra.Command, args []string) error {
		path := rt.resolveBackupFile(args[0])
		rec, err := backup.Load(path)
		if err == nil {
			err = checkBackupTarget(rec, rt.Cfg.Profile, rt.Cfg.BaseURL, f.ignoreTarget)
		} else {
			err = backupFileError(err, path)
		}
		if err != nil {
			rt.Command = safety.CmdBackupsRestore
			rt.emit(output.NewError(safety.CmdBackupsRestore, err))
			return nil
		}
		f.rec, f.path = rec, path
		return data(c, args)
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.recreate, "recreate", false, "create the document again when it no longer exists (the id changes)")
	fl.BoolVar(&f.ignoreTarget, "ignore-target", false, "restore even though the backup came from another profile or server")
	fl.BoolVar(&f.asDraft, "draft", false, "save the backup as a new draft version, whatever it holds")
	fl.BoolVar(&f.publish, "publish", false, "restore and publish (_status=published)")
	fl.IntVar(&f.depth, "depth", 0, "relationship expansion depth of the echoed document")
	fl.BoolVar(&f.noEchoCheck, "no-echo-check", false, "skip the §10.2 echo-diff")

	SetHelp(cmd, &Help{
		Synopsis: []string{
			"pay backups restore <file> [--dry-run] [--yes]",
			"pay backups restore <file> --recreate --yes          # the document was permanently deleted",
			"pay backups restore <file> [--draft|--publish] [--ignore-target]",
		},
		Args: []ArgSpec{{Name: "file", Required: true, Type: "string",
			Example: "a path from `pay backups list`"}},
		FlagInfo: map[string]FlagInfo{
			"recreate":      {Note: "POST a new document when the original is gone; Payload assigns a new id"},
			"ignore-target": {Note: "the backup's profile/base_url differ from the current ones; restore anyway"},
			"draft":         {Note: "only on a drafts-enabled collection or global"},
		},
		Output: OutputSpec{Kind: output.KindDoc,
			Skeleton: `{"id":16,"title":"…as it was…"}  · dry run: op_result {"would_affect":1,"request":{…},"would_backup":{…}}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitAuth, apierr.ExitNotFound,
			apierr.ExitValidation, apierr.ExitNetwork, apierr.ExitAccessDenied, apierr.ExitConfig,
			apierr.ExitCapability, apierr.ExitConfirmationRequired},
		Examples: []Example{
			{Why: "always first: the exact request, and the backup the restore itself takes",
				Cmd: "pay backups restore ~/pay-backups/local/pages/16/20260928T120000.000Z--update.json --dry-run"},
			{Why: "undo an update", Cmd: "pay backups restore <file> --yes"},
			{Why: "bring back a permanently deleted document (new id)", Cmd: "pay backups restore <file> --recreate --yes"},
			{Why: "put the old content back as a draft, leaving the live page alone", Cmd: "pay backups restore <file> --draft --yes"},
			{Why: "restore a global", Cmd: "pay backups restore ~/pay-backups/local/_globals/header/20260928T120000.000Z--globals-update.json --yes"},
		},
		Mistakes: []Mistake{
			{Wrong: "Restoring a stage backup while the default profile points at production.",
				Right: "PayCLI refuses (backup_target_mismatch, exit 5) when the backup's profile or base_url differ. Switch --profile; --ignore-target is for a deliberate copy."},
			{Wrong: "Expecting --recreate to keep the id.",
				Right: "Payload ignores a client-supplied id on create. The new id is in data.id and the warning; fix relationships that pointed at the old one."},
			{Wrong: "Expecting a restore to bring back upload file bytes.",
				Right: "Only the document is backed up. The file itself (upload --replace) is not."},
			{Wrong: "Running it without --yes in a script and treating exit 11 as failure.",
				Right: "It overwrites a whole document, so it is L2 like `versions restore`: pass --yes after --dry-run."},
		},
		SeeAlso: []string{"pay backups show <file>", "pay versions list <collection> --id <id>", "pay get <collection> <id>"},
	})
	return cmd
}

func runBackupsRestore(ctx context.Context, d *Deps, f *backupsRestoreFlags) (*output.Envelope, error) {
	if f.asDraft && f.publish {
		return nil, apierr.New(apierr.CodeInvalidArgs, "--draft and --publish are mutually exclusive")
	}
	client, err := d.requireClient()
	if err != nil {
		return nil, err
	}
	rec, path := f.rec, f.path
	if rec.Global != "" {
		return restoreGlobalBackup(ctx, d, client, f, rec, path)
	}
	return restoreDocBackup(ctx, d, client, f, rec, path)
}

// checkBackupTarget refuses to write a backup into a different place than the
// one it was taken from.
func checkBackupTarget(rec *backup.Record, profile, baseURL string, ignore bool) error {
	if ignore {
		return nil
	}
	here := redact.URL(baseURL)
	if rec.Profile == profile && rec.BaseURL == here {
		return nil
	}
	return apierr.New(apierr.CodeBackupTargetMismatch,
		"this backup was taken from profile %q at %s, and this command would write to profile %q at %s",
		rec.Profile, rec.BaseURL, profile, here).
		WithHint("re-run with --profile %s, or pass --ignore-target if restoring it here is really intended", rec.Profile)
}

// serverOwnedRestoreFields are never sent back: the server sets them, and
// sending them is at best ignored and at worst a validation error.
var serverOwnedRestoreFields = []string{"id", "createdAt", "updatedAt", "sizes", "thumbnailURL", "globalType"}

// restoreBody is the body a restore sends: the stored document minus
// server-owned, read-only, join and redacted fields.
//
// Server-owned fields (id, timestamps, upload-derived sizes) and join fields
// (computed from OTHER documents) are dropped silently: there is nothing to
// restore in them. What is returned as notRestored is the part a caller could
// actually miss — redacted values, and fields the schema marks read-only for
// a reason PayCLI cannot see — so the envelope can say exactly that.
func restoreBody(rec *backup.Record, shard *discovery.Shard, upload bool) (map[string]any, []string) {
	return restoreBodyOf(rec.Doc, shard, upload)
}

// restoreBodyOf is restoreBody for one stored document (doc or published_doc).
func restoreBodyOf(doc map[string]any, shard *discovery.Shard, upload bool) (map[string]any, []string) {
	body, _ := jsonValue(doc).(map[string]any)
	if body == nil {
		body = map[string]any{}
	}
	var notRestored []string
	drop := func(key string, report bool) {
		if _, ok := body[key]; ok {
			delete(body, key)
			if report {
				notRestored = append(notRestored, key)
			}
		}
	}
	for _, k := range serverOwnedRestoreFields {
		drop(k, false)
	}
	if upload {
		// Upload-derived and virtual; the file is not part of the backup.
		drop("url", false)
	}
	if shard != nil {
		joins := map[string]bool{}
		for _, j := range shard.JoinFields {
			joins[j] = true
			drop(j, false)
		}
		for _, fld := range shard.Fields {
			if fld.Parent != nil || !fld.ReadOnly || joins[fld.Path] {
				continue
			}
			if fld.Path == "_status" || fld.Path == "deletedAt" {
				continue
			}
			drop(fld.Path, true)
		}
	}
	notRestored = append(notRestored, dropMasked(body, "")...)
	sortStrings(notRestored)
	return body, notRestored
}

// dropMasked removes every value §5.3 redacted when the backup was written, so
// the mask string is never written into a real document.
func dropMasked(v any, path string) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if s, ok := val.(string); ok && s == redact.Mask {
				delete(t, k)
				out = append(out, p)
				continue
			}
			out = append(out, dropMasked(val, p)...)
		}
	case []any:
		for i, val := range t {
			out = append(out, dropMasked(val, fmt.Sprintf("%s[%d]", path, i))...)
		}
	}
	return out
}

// notRestoredWarning names what the restore left out, so "restored" is never
// read as "every byte of the backup is back".
func notRestoredWarning(notRestored []string) *output.Warning {
	if len(notRestored) == 0 {
		return nil
	}
	return &output.Warning{
		Code: WarnFieldsNotRestored,
		Message: "these values from the backup were not sent: they were redacted when the backup was written, " +
			"or the schema marks them read-only",
		Paths: notRestored,
		Hint:  "a redacted credential (apiKey, a user's sessions or hash…) keeps its current value; set it deliberately if it must change",
	}
}

// restoreMode is how a backup is written back to a drafts-enabled entity.
type restoreMode struct {
	// draft saves with ?draft=true (the final request, in a two-step restore).
	draft bool
	// status is the _status to send ("" leaves the body's own value).
	status string
	// asDraftWarn: a draft backup saved on top of a live published document.
	asDraftWarn bool
	// twoStep: the backup holds a published state (published_doc) under a
	// newer draft. The published state is written back first — a plain save
	// with _status "published" — and then the draft on top with draft=true,
	// which reproduces exactly what was there: the page live with the old
	// published content, and the old draft pending.
	twoStep bool
}

// restoreDraftMode decides how a drafts-enabled entity is written back.
//
//   - --draft / --publish decide alone.
//   - A backup with a published_doc (a published document with a newer
//     draft) is restored in two steps (restoreMode.twoStep).
//   - A draft backup is saved with draft=true, as the draft it was: the save
//     skips required-field validation exactly like the original draft save
//     did (payload@3.87.1 collections/operations/utilities/update.js: only
//     isSavingDraft skips validation), so an incomplete work-in-progress draft
//     can be restored. Over a live published document that is a new draft on
//     top (restored_as_draft); never an unpublish. Payload does not update the
//     main row on a draft save, so a plain `pay get` of a never-published
//     document may still show the older main row; `--draft` shows the restore.
//   - Anything else — a published backup — is a plain save.
func restoreDraftMode(f *backupsRestoreFlags, drafts *bool, backupStatus, liveStatus string, hasPublished bool) restoreMode {
	if drafts == nil || !*drafts {
		return restoreMode{}
	}
	switch {
	case f.asDraft:
		return restoreMode{draft: true, status: "draft"}
	case f.publish:
		return restoreMode{status: "published"}
	case hasPublished:
		return restoreMode{draft: true, status: "draft", twoStep: true}
	case backupStatus == "draft":
		// Writing _status=draft to the main row would take a live page
		// offline; a draft save leaves the main row alone.
		return restoreMode{draft: true, status: "draft", asDraftWarn: liveStatus == "published"}
	}
	return restoreMode{}
}

// publishedNotRestoredWarning is --draft (or a failed first step) over a
// backup whose published state was not put back.
func publishedNotRestoredWarning(file string, liveStatus string) *output.Warning {
	if liveStatus == "published" {
		return nil
	}
	return &output.Warning{
		Code: WarnPublishedNotRestored,
		Message: "the backup also holds the document's published state (published_doc), which --draft does not restore; " +
			"the document is still unpublished",
		Hint: "pay backups restore " + shellQuote(file) + " --yes   # restores the published state and the draft on top",
	}
}

// restoreStep is one request of a multi-request restore in a dry run.
type restoreStep struct {
	Step   string          `json:"step"`
	Method string          `json:"method"`
	URL    string          `json:"url"`
	Body   json.RawMessage `json:"body,omitempty"`
	// Skipped: the live document already holds this state, so the real run
	// does not send it.
	Skipped bool `json:"skipped,omitempty"`
}

func newRestoreStep(d *Deps, step, method, url string, body map[string]any, skipped bool) restoreStep {
	st := restoreStep{Step: step, Method: method, URL: redact.URL(url), Skipped: skipped}
	if b, err := json.Marshal(body); err == nil {
		if d.cfg().Redact {
			b = redact.JSON(b).Data
		}
		st.Body = b
	}
	return st
}

func restoredAsDraftWarning(file string) output.Warning {
	return output.Warning{
		Code: WarnRestoredAsDraft,
		Message: "the backup holds a draft and the live document is published, so the backup was saved as a NEW DRAFT " +
			"on top of it; the published version is unchanged",
		Hint: "pay backups restore " + shellQuote(file) + " --publish --yes   # to publish the backup's content instead",
	}
}

func restoreDocBackup(ctx context.Context, d *Deps, client *payload.Client, f *backupsRestoreFlags,
	rec *backup.Record, file string) (*output.Envelope, error) {
	cfg := d.cfg()
	t, err := d.collection(ctx, rec.Collection)
	if err != nil {
		return nil, err
	}
	id := rec.IDString()
	if id == "" {
		return nil, apierr.New(apierr.CodeBadRequestBody, "%s is a collection backup with no document id", file)
	}
	flags := t.flags()
	body, dropped := restoreBody(rec, t.Shard, flags.Upload != nil && *flags.Upload)

	p := query.Params{Depth: query.IntPtr(f.depth), Locale: rec.Locale, FallbackLocale: rec.FallbackLocale}
	if flags.Trash != nil && *flags.Trash {
		// A trashed document is only addressable with trash=true, and a live
		// one still is (verified on Payload 3.87).
		p.Trash = query.BoolPtr(true)
	}

	// The live state: does the document still exist, and is it published?
	// No draft=true here on purpose — the main row's _status is the one a
	// restore could take offline.
	live, _, getErr := client.Get(ctx, t.Slug, id, query.Params{Depth: query.IntPtr(0), Trash: p.Trash,
		Locale: rec.Locale, FallbackLocale: rec.FallbackLocale}, payload.WithKnownRoute())
	gone := getErr != nil && apierr.HasCode(getErr, apierr.CodeDocNotFound)
	if getErr != nil && !gone {
		return nil, getErr
	}
	if gone {
		if !f.recreate {
			return nil, apierr.New(apierr.CodeDocNotFound,
				"%s %s no longer exists, so the backup cannot be written back over it", t.Slug, id).
				WithHint("pay backups restore %s --recreate --dry-run   # creates it again; Payload assigns a NEW id", shellQuote(file))
		}
		return recreateDocBackup(ctx, d, client, f, t, rec, file, body, dropped, p)
	}

	var warnings []output.Warning
	if f.recreate {
		warnings = append(warnings, output.Warning{
			Code:    WarnRecreateIgnored,
			Message: fmt.Sprintf("%s %s still exists, so it was restored in place and --recreate was ignored", t.Slug, id),
		})
	}
	backupStatus, _ := rec.Doc["_status"].(string)
	liveStatus, _ := live["_status"].(string)
	if f.asDraft || f.publish {
		if err := checkDrafts(t.Slug, flags.Drafts); err != nil {
			return nil, err
		}
	}
	mode := restoreDraftMode(f, flags.Drafts, backupStatus, liveStatus, rec.PublishedDoc != nil)
	if mode.draft {
		p.Draft = query.BoolPtr(true)
	}
	if mode.status != "" {
		body["_status"] = mode.status
	}
	if mode.asDraftWarn {
		warnings = append(warnings, restoredAsDraftWarning(file))
	}
	if rec.PublishedDoc != nil && f.asDraft {
		if w := publishedNotRestoredWarning(file, liveStatus); w != nil {
			warnings = append(warnings, *w)
		}
	}

	// Two-step restore: the published state first (skipped when the live
	// main row already holds it), then the draft on top.
	var pubBody map[string]any
	pubParams := p
	pubParams.Draft = nil
	skipPub := true
	if mode.twoStep {
		var pubDropped []string
		pubBody, pubDropped = restoreBodyOf(rec.PublishedDoc, t.Shard, flags.Upload != nil && *flags.Upload)
		pubBody["_status"] = "published"
		dropped = mergeStrings(dropped, pubDropped)
		skipPub = liveStatus == "published" && docdiff.DiffPatch(map[string]any(live), pubBody, docdiff.Options{}).Identical
	}
	if w := notRestoredWarning(dropped); w != nil {
		warnings = append(warnings, *w)
	}

	op := safety.Op{Command: safety.CmdBackupsRestore, Selector: safety.SelectorID}
	docPath := "/" + t.Slug + "/" + id
	newOp := func(step string) *writeOp {
		w := d.newWriteOp(op, t.Slug, "PATCH", docPath).withIDs([]any{id}).
			withBackup(ctx, BackupTarget{Collection: t, Params: p}).withRestoredFrom(file)
		w.step = step
		return w
	}
	w := newOp("")
	if mode.twoStep {
		w.step = "backups restore 2/2: the draft on top"
	}
	o := orphanTarget{t: t, id: id}
	send := func(b map[string]any, bp query.Params) (*payload.WriteResult, error) {
		return client.Update(ctx, t.Slug, id, b, bp,
			payload.WithClassify(classifyFor(t, cfg, payload.SentPaths(b))), payload.WithKnownRoute())
	}
	if cfg.DryRun {
		// F7 stage B: what the restore changes, against the newest version
		// (the state Payload merges the restored body into).
		diff, pre, dw := d.dryRunDiff(ctx, client, o, body, p, diffBaseLatest,
			fmt.Sprintf("after the restore, repair with: pay update %s %s --reset-nested --dry-run", t.Slug, shellQuote(id)))
		warnings = append(warnings, dw...)
		if mode.twoStep && pre != nil {
			diff = d.twoStepRestoreDiff(o, pre, pubBody, body, skipPub)
			warnings = append(warnings, twoStepRestoreWarning(o, skipPub))
		}
		if diff != nil {
			w.dryDiff = diff
		}
		q, _ := p.Encode()
		url := client.URLFor(&payload.Request{Method: "PATCH", Path: docPath, Query: q})
		if mode.twoStep {
			pq, _ := pubParams.Encode()
			w.drySteps = restoreStepsPreview(d, client.URLFor(&payload.Request{Method: "PATCH", Path: docPath, Query: pq}),
				pubBody, skipPub, url, body)
		}
		env, err := emitDryRun(d, w, safety.CmdBackupsRestore, "PATCH", url, body, 1, []any{id}, warnings...)
		if err != nil {
			return nil, err
		}
		env.WithTarget(t.envTarget(rec.ID))
		return env, nil
	}
	if err := w.confirm(1); err != nil {
		return nil, err
	}
	if mode.twoStep && !skipPub {
		w1 := newOp("backups restore 1/2: the published state")
		if err := w1.pre(1); err != nil {
			return nil, err
		}
		res1, err := send(pubBody, pubParams)
		if err != nil {
			w1.post(false, statusOf(res1), 0, 1, err.Error(), "")
			return nil, apierr.Wrap(err, apierr.CodeOf(err),
				"restoring the backup's published state (step 1 of 2) failed, so nothing was written: %s", errMessage(err)).
				WithHint("pay backups restore %s --draft --yes   # restores only the draft, leaving the live document as it is", shellQuote(file))
		}
		w1.post(true, statusOf(res1), 1, 0, "", requestIDOf(res1))
	}
	if err := w.pre(1); err != nil {
		return nil, err
	}
	res, err := send(body, p)
	if err != nil {
		w.post(false, statusOf(res), 0, 1, err.Error(), "")
		if mode.twoStep && !skipPub {
			return nil, apierr.Wrap(err, apierr.CodeOf(err),
				"the backup's published state was restored (the document is live again), but saving its draft on top "+
					"(step 2 of 2) failed: %s", errMessage(err)).
				WithHint("pay backups restore %s --draft --yes   # saves the backup's draft on top again", shellQuote(file))
		}
		return nil, err
	}
	env := output.New(safety.CmdBackupsRestore, output.KindDoc, res.Doc).
		WithTarget(t.envTarget(res.Doc.ID())).
		WithChanged(&output.Changed{Updated: 1, IDs: []any{res.Doc.ID()}}).
		WithMeta(withLocaleMeta(w.run.meta(), p)).
		WithRawBody(res.Raw, !cfg.Redact).
		WithNext(&output.Next{
			Reason: output.ReasonVerifyWrite,
			Cmd: fmt.Sprintf("pay get %s %s --depth 0%s%s%s", t.Slug, res.Doc.IDString(), draftFlag(mode.draft),
				trashFlag(res.Doc), d.profileFlag()),
		})
	env.Meta.RestoredFrom = file
	for _, warn := range warnings {
		env.AddWarning(warn)
	}
	for _, warn := range echoWarnings(t.Slug, body, res.Doc, nil, echoConfig{
		Shard: t.Shard, Ignore: cfg.EchoCheckIgnore, LocaleAll: p.Locale == "all", Disabled: f.noEchoCheck,
	}) {
		env.AddWarning(warn)
	}
	warns, _ := d.orphanCheckAfterWrite(ctx, client, o, body, res.Doc, p, f.noEchoCheck, false)
	for _, warn := range warns {
		env.AddWarning(warn)
	}
	return w.finish(env, res.HTTP, 1, 0, "")
}

// twoStepRestoreDiff is data.diff for a restore that writes the backup's
// published state (step 1, no ?draft: Payload merges it into the newest
// version and stores the result as the live row) and then its draft on top
// (step 2, ?draft=true: merged into the step-1 result, saved as a version
// only). A single diff of the draft body against the current newest version
// hid step 1 entirely — the live content change was in no diff, and the
// `_status published → draft` line read like an unpublish although the
// document stays published. So:
//
//   - data.diff.published: the stored (live) row → the step-1 result — what
//     visitors will see change. Absent when step 1 is skipped (the live row
//     already holds the backup's published state).
//   - data.diff: the current newest version → the newest version after both
//     steps (the backup's draft), base "latest".
func (d *Deps) twoStepRestoreDiff(o orphanTarget, pre *preimage, pubBody, draftBody map[string]any,
	skipPub bool) map[string]any {
	afterPub := map[string]any(pre.latest)
	var published map[string]any
	if !skipPub {
		afterPub = docdiff.ApplyPatch(map[string]any(pre.latest), pubBody, docdiff.Options{})
		published = d.diffData(o, diffBasePublished, orphans.WithoutEmptyRows(map[string]any(pre.stored)), afterPub)
	}
	out := d.diffData(o, diffBaseLatest, map[string]any(pre.latest),
		docdiff.ApplyPatch(afterPub, draftBody, docdiff.Options{}))
	if published != nil {
		out["published"] = published
	}
	return out
}

// twoStepRestoreWarning says, in the dry run, what a two-step restore leaves
// live and what it leaves as the draft.
func twoStepRestoreWarning(o orphanTarget, skipPub bool) output.Warning {
	msg := fmt.Sprintf("the backup holds a published state and a newer draft, so the restore writes two versions: "+
		"step 1 writes the backup's published state into the live row of %s (data.diff.published), step 2 saves "+
		"the backup's draft on top as the newest version (data.diff); the document stays published", o.what())
	if skipPub {
		msg = fmt.Sprintf("the backup holds a published state and a newer draft; the live row of %s already holds "+
			"that published state, so only step 2 runs: the backup's draft is saved on top (data.diff) and the document "+
			"stays published", o.what())
	}
	return output.Warning{
		Code:    WarnRestoreTwoSteps,
		Message: msg,
		Hint:    "data.steps lists both requests; --draft restores only the draft and leaves the live document as it is",
	}
}

// restoreStepsPreview is the dry run's data.steps for a two-step restore:
// both requests, in order.
func restoreStepsPreview(d *Deps, pubURL string, pubBody map[string]any, skipPub bool,
	draftURL string, draftBody map[string]any) []restoreStep {
	return []restoreStep{
		newRestoreStep(d, "1/2 the published state (published_doc), a plain save", "PATCH", pubURL, pubBody, skipPub),
		newRestoreStep(d, "2/2 the draft on top (doc), saved with draft=true", "PATCH", draftURL, draftBody, false),
	}
}

// mergeStrings is the sorted union of a and b.
func mergeStrings(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string(nil), a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sortStrings(out)
	return out
}

func draftFlag(draft bool) string {
	if draft {
		return " --draft"
	}
	return ""
}

// trashFlag is " --trash" for a document that is in the trash, which a plain
// `pay get` cannot see.
func trashFlag(doc payload.Doc) string {
	if v, ok := doc["deletedAt"]; ok && v != nil {
		return " --trash"
	}
	return ""
}

// recreateDocBackup is --recreate: the original is gone, so POST it again.
func recreateDocBackup(ctx context.Context, d *Deps, client *payload.Client, f *backupsRestoreFlags,
	t *collTarget, rec *backup.Record, file string, body map[string]any, dropped []string,
	p query.Params) (*output.Envelope, error) {
	cfg := d.cfg()
	flags := t.flags()
	var warnings []output.Warning
	// twoStep: the backup holds a published state under a newer draft. The
	// document is created from the published state, and the draft is then
	// saved on top of the NEW id — the same shape it had.
	var draftBody map[string]any
	if flags.Drafts != nil && *flags.Drafts {
		status, _ := body["_status"].(string)
		switch {
		case f.publish:
			body["_status"] = "published"
		case f.asDraft || (status == "draft" && rec.PublishedDoc == nil):
			// Nothing is live to protect; a draft create skips required-field
			// validation exactly like the original draft save did.
			body["_status"] = "draft"
			p.Draft = query.BoolPtr(true)
		case rec.PublishedDoc != nil:
			draftBody = body
			draftBody["_status"] = "draft"
			var pubDropped []string
			body, pubDropped = restoreBodyOf(rec.PublishedDoc, t.Shard, flags.Upload != nil && *flags.Upload)
			body["_status"] = "published"
			dropped = mergeStrings(dropped, pubDropped)
		}
	}
	p.Trash = nil
	if w := notRestoredWarning(dropped); w != nil {
		warnings = append(warnings, *w)
	}
	oldID := rec.IDString()
	op := safety.Op{Command: safety.CmdBackupsRestore, Selector: safety.SelectorNone}
	w := d.newWriteOp(op, t.Slug, "POST", "/"+t.Slug).withRestoredFrom(file)
	if draftBody != nil {
		w.step = "backups restore 1/2: create from the published state"
	}
	draftParams := p
	draftParams.Draft = query.BoolPtr(true)
	if cfg.DryRun {
		q, _ := p.Encode()
		warnings = append(warnings, output.Warning{
			Code: WarnRecreatedNewID,
			Message: fmt.Sprintf("%s %s no longer exists; the real run CREATES a new document, and Payload assigns it a new id",
				t.Slug, oldID),
			Hint: "relationships in other documents that pointed at id " + oldID + " are not updated",
		})
		url := client.URLFor(&payload.Request{Method: "POST", Path: "/" + t.Slug, Query: q})
		if draftBody != nil {
			dq, _ := draftParams.Encode()
			w.drySteps = []restoreStep{
				newRestoreStep(d, "1/2 create from the published state (published_doc)", "POST", url, body, false),
				newRestoreStep(d, "2/2 the draft on top (doc), saved with draft=true on the new id", "PATCH",
					client.URLFor(&payload.Request{Method: "PATCH", Path: "/" + t.Slug + "/<new id>", Query: dq}), draftBody, false),
			}
		}
		return emitDryRun(d, w, safety.CmdBackupsRestore, "POST", url, body, 1, nil, warnings...)
	}
	if err := w.confirm(1); err != nil {
		return nil, err
	}
	if err := w.pre(1); err != nil {
		return nil, err
	}
	res, err := client.Create(ctx, t.Slug, body, p,
		payload.WithClassify(classifyFor(t, cfg, payload.SentPaths(body))), payload.WithKnownRoute())
	if err != nil {
		w.post(false, statusOf(res), 0, 1, err.Error(), "")
		return nil, err
	}
	newID := res.Doc.IDString()
	if draftBody != nil {
		// Step 1 is committed: record it, then save the draft on the new id.
		w.withIDs([]any{res.Doc.ID()}).post(true, statusOf(res), 1, 0, "", requestIDOf(res))
		w = d.newWriteOp(safety.Op{Command: safety.CmdBackupsRestore, Selector: safety.SelectorID}, t.Slug, "PATCH",
			"/"+t.Slug+"/"+newID).withIDs([]any{res.Doc.ID()}).withRestoredFrom(file)
		w.step = "backups restore 2/2: the draft on top"
		if err := w.pre(1); err != nil {
			return nil, err
		}
		res2, err := client.Update(ctx, t.Slug, newID, draftBody, draftParams,
			payload.WithClassify(classifyFor(t, cfg, payload.SentPaths(draftBody))), payload.WithKnownRoute())
		if err != nil {
			w.post(false, statusOf(res2), 0, 1, err.Error(), "")
			return nil, apierr.Wrap(err, apierr.CodeOf(err),
				"%s %s was re-created as id %s from the backup's published state, but saving its draft on top "+
					"(step 2 of 2) failed: %s", t.Slug, oldID, newID, errMessage(err)).
				WithHint("pay update %s %s --data-file <the backup's .doc> --draft --yes   # the draft; `pay backups show %s --path .doc` prints it",
					t.Slug, newID, shellQuote(file))
		}
		res.HTTP = res2.HTTP
		p = draftParams
		body = draftBody
		res = res2
	}
	if trashFlag(res.Doc) != "" {
		warnings = append(warnings, output.Warning{
			Code: WarnRecreatedInTrash,
			Message: fmt.Sprintf("the backup was taken while %s %s was in the trash, so it was re-created in the trash",
				t.Slug, oldID),
			Hint: fmt.Sprintf("pay restore %s %s --yes   # take it out of the trash", t.Slug, newID),
		})
	}
	if newID != oldID {
		warnings = append(warnings, output.Warning{
			Code: WarnRecreatedNewID,
			Message: fmt.Sprintf("%s %s was re-created as id %s: the original id is gone for good, and relationships "+
				"in other documents that pointed at %s were NOT updated", t.Slug, oldID, newID, oldID),
			Hint: fmt.Sprintf("pay find <collection> --where '<field> equals %s'   # find what still points at the old id", oldID),
		})
	}
	env := output.New(safety.CmdBackupsRestore, output.KindDoc, res.Doc).
		WithTarget(t.envTarget(res.Doc.ID())).
		WithChanged(&output.Changed{Created: 1, IDs: []any{res.Doc.ID()}}).
		WithMeta(withLocaleMeta(w.run.meta(), p)).
		WithRawBody(res.Raw, !cfg.Redact).
		WithNext(&output.Next{
			Reason: output.ReasonVerifyWrite,
			Cmd: fmt.Sprintf("pay get %s %s --depth 0%s%s%s", t.Slug, newID, draftFlag(p.Draft != nil),
				trashFlag(res.Doc), d.profileFlag()),
		})
	env.Meta.RestoredFrom = file
	for _, warn := range warnings {
		env.AddWarning(warn)
	}
	for _, warn := range echoWarnings(t.Slug, body, res.Doc, nil, echoConfig{
		Shard: t.Shard, Ignore: cfg.EchoCheckIgnore, LocaleAll: p.Locale == "all", Disabled: f.noEchoCheck,
	}) {
		env.AddWarning(warn)
	}
	return w.withIDs([]any{res.Doc.ID()}).finish(env, res.HTTP, 1, 0, "")
}

func restoreGlobalBackup(ctx context.Context, d *Deps, client *payload.Client, f *backupsRestoreFlags,
	rec *backup.Record, file string) (*output.Envelope, error) {
	cfg := d.cfg()
	if f.recreate {
		return nil, apierr.New(apierr.CodeInvalidArgs, "--recreate does not apply to a global: a global always exists").
			WithHint("drop --recreate: pay backups restore %s --yes", shellQuote(file))
	}
	g, err := d.global(ctx, rec.Global)
	if err != nil {
		return nil, err
	}
	flags := g.flags()
	if f.asDraft || f.publish {
		if err := checkDrafts(g.Slug, flags.Drafts); err != nil {
			return nil, err
		}
	}
	body, dropped := restoreBody(rec, g.Shard, false)
	p := query.Params{Depth: query.IntPtr(f.depth), Locale: rec.Locale, FallbackLocale: rec.FallbackLocale}

	var warnings []output.Warning
	liveStatus := ""
	var live payload.Doc
	if flags.Drafts != nil && *flags.Drafts {
		var err error
		live, _, err = client.GlobalGet(ctx, g.Slug, query.Params{Depth: query.IntPtr(0),
			Locale: rec.Locale, FallbackLocale: rec.FallbackLocale}, payload.WithKnownRoute())
		if err != nil {
			return nil, err
		}
		liveStatus, _ = live["_status"].(string)
	}
	backupStatus, _ := rec.Doc["_status"].(string)
	mode := restoreDraftMode(f, flags.Drafts, backupStatus, liveStatus, rec.PublishedDoc != nil)
	if mode.draft {
		p.Draft = query.BoolPtr(true)
	}
	if mode.status != "" {
		body["_status"] = mode.status
	}
	if mode.asDraftWarn {
		warnings = append(warnings, restoredAsDraftWarning(file))
	}
	if rec.PublishedDoc != nil && f.asDraft {
		if w := publishedNotRestoredWarning(file, liveStatus); w != nil {
			warnings = append(warnings, *w)
		}
	}
	var pubBody map[string]any
	pubParams := p
	pubParams.Draft = nil
	skipPub := true
	if mode.twoStep {
		var pubDropped []string
		pubBody, pubDropped = restoreBodyOf(rec.PublishedDoc, g.Shard, false)
		pubBody["_status"] = "published"
		dropped = mergeStrings(dropped, pubDropped)
		skipPub = liveStatus == "published" && docdiff.DiffPatch(map[string]any(live), pubBody, docdiff.Options{}).Identical
	}
	if w := notRestoredWarning(dropped); w != nil {
		warnings = append(warnings, *w)
	}

	op := safety.Op{Command: safety.CmdBackupsRestore, Selector: safety.SelectorNone, Global: true}
	path := payload.GlobalPath(g.Slug)
	newOp := func(step string) *writeOp {
		w := d.newWriteOp(op, g.Slug, "POST", path).withBackup(ctx, BackupTarget{Global: g, Params: p}).
			withRestoredFrom(file)
		w.global = true
		w.step = step
		return w
	}
	w := newOp("")
	if mode.twoStep {
		w.step = "backups restore 2/2: the draft on top"
	}
	send := func(b map[string]any, bp query.Params) (*payload.WriteResult, error) {
		return client.GlobalUpdate(ctx, g.Slug, b, bp,
			payload.WithClassify(payload.ClassifyContext{SentPaths: payload.SentPaths(b), IncludeRaw: true, NoRedact: !cfg.Redact}),
			payload.WithKnownRoute())
	}
	o := orphanTarget{g: g}
	if cfg.DryRun {
		diff, pre, dw := d.dryRunDiff(ctx, client, o, body, p, diffBaseLatest,
			"after the restore, repair with: pay globals update "+g.Slug+" --reset-nested --dry-run")
		warnings = append(warnings, dw...)
		if mode.twoStep && pre != nil {
			diff = d.twoStepRestoreDiff(o, pre, pubBody, body, skipPub)
			warnings = append(warnings, twoStepRestoreWarning(o, skipPub))
		}
		if diff != nil {
			w.dryDiff = diff
		}
		q, _ := p.Encode()
		url := client.URLFor(&payload.Request{Method: "POST", Path: path, Query: q})
		if mode.twoStep {
			pq, _ := pubParams.Encode()
			w.drySteps = restoreStepsPreview(d, client.URLFor(&payload.Request{Method: "POST", Path: path, Query: pq}),
				pubBody, skipPub, url, body)
		}
		env, err := emitDryRun(d, w, safety.CmdBackupsRestore, "POST", url, body, 1, nil, warnings...)
		if err != nil {
			return nil, err
		}
		env.WithTarget(g.envTarget())
		return env, nil
	}
	if err := w.confirm(1); err != nil {
		return nil, err
	}
	if mode.twoStep && !skipPub {
		w1 := newOp("backups restore 1/2: the published state")
		if err := w1.pre(1); err != nil {
			return nil, err
		}
		res1, err := send(pubBody, pubParams)
		if err != nil {
			w1.post(false, statusOf(res1), 0, 1, err.Error(), "")
			return nil, apierr.Wrap(err, apierr.CodeOf(err),
				"restoring the backup's published state (step 1 of 2) failed, so nothing was written: %s", errMessage(err)).
				WithHint("pay backups restore %s --draft --yes   # restores only the draft, leaving the live global as it is", shellQuote(file))
		}
		w1.post(true, statusOf(res1), 1, 0, "", requestIDOf(res1))
	}
	if err := w.pre(1); err != nil {
		return nil, err
	}
	res, err := send(body, p)
	if err != nil {
		w.post(false, statusOf(res), 0, 1, err.Error(), "")
		if mode.twoStep && !skipPub {
			return nil, apierr.Wrap(err, apierr.CodeOf(err),
				"the backup's published state was restored (the global is live again), but saving its draft on top "+
					"(step 2 of 2) failed: %s", errMessage(err)).
				WithHint("pay backups restore %s --draft --yes   # saves the backup's draft on top again", shellQuote(file))
		}
		return nil, err
	}
	env := output.New(safety.CmdBackupsRestore, output.KindGlobal, res.Doc).
		WithTarget(g.envTarget()).
		WithChanged(&output.Changed{Updated: 1, IDs: []any{}}).
		WithMeta(w.run.meta()).
		WithRawBody(res.Raw, !cfg.Redact).
		WithNext(&output.Next{
			Reason: output.ReasonVerifyWrite,
			Cmd:    fmt.Sprintf("pay globals get %s --depth 0%s%s", g.Slug, draftFlag(mode.draft), d.profileFlag()),
		})
	env.Meta.RestoredFrom = file
	for _, warn := range warnings {
		env.AddWarning(warn)
	}
	for _, warn := range echoWarnings(g.Slug, body, res.Doc, nil, echoConfig{
		Shard: g.Shard, Ignore: cfg.EchoCheckIgnore, LocaleAll: p.Locale == "all", Disabled: f.noEchoCheck,
	}) {
		env.AddWarning(warn)
	}
	warns, _ := d.orphanCheckAfterWrite(ctx, client, o, body, res.Doc, p, f.noEchoCheck, false)
	for _, warn := range warns {
		env.AddWarning(warn)
	}
	return w.finish(env, res.HTTP, 1, 0, "")
}

// ---------------------------------------------------------------------------
// prune
// ---------------------------------------------------------------------------

// pruneSample caps how many paths the prune envelope lists.
const pruneSample = 20

type backupsPruneData struct {
	Dir         string         `json:"dir"`
	Cutoff      time.Time      `json:"cutoff"`
	DryRun      bool           `json:"dry_run"`
	Removed     int            `json:"removed"`
	WouldRemove int            `json:"would_remove"`
	Bytes       int64          `json:"bytes"`
	Sample      []backup.Entry `json:"sample"`
	Truncated   bool           `json:"truncated"`
}

func newBackupsPruneCmd(rt *Runtime) *cobra.Command {
	var (
		olderThan   string
		collection  string
		global      string
		id          string
		allProfiles bool
	)
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete backup files older than a cutoff",
		Long: `Delete the backup files of the current profile that are older than --older-than,
and any directory that leaves empty. It is L2 — the backups are the safety net —
so it asks on a TTY and needs --yes otherwise; --dry-run lists what would go.`,
		Args: maxArgs(0, "pay backups prune --older-than D [--dry-run] [--yes]"),
		RunE: Handle(rt, "backups prune", func(_ context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			if strings.TrimSpace(olderThan) == "" {
				return nil, apierr.New(apierr.CodeInvalidArgs, "pay backups prune needs --older-than").
					WithHint("pay backups prune --older-than 30d --dry-run")
			}
			cutoff, err := query.ParseSince(olderThan, rt.Now())
			if err != nil {
				return nil, err
			}
			dir := rt.backupsDirOrWarn()
			store := backup.New(dir)
			f := backup.Filter{Collection: collection, Global: global, ID: id, Before: cutoff}
			if !allProfiles {
				f.Profile = rt.Cfg.Profile
			}
			candidates, err := store.List(f)
			if err != nil {
				return nil, apierr.Wrap(err, apierr.CodeInternal, "cannot read the backup directory %s: %v", dir, err)
			}
			dry := rt.Cfg.DryRun
			if !dry && len(candidates) > 0 {
				req := safety.Request{
					Op:       safety.Op{Command: safety.CmdBackupsPrune},
					Affected: len(candidates),
					Summary: fmt.Sprintf("backups prune will delete %d backup file(s) older than %s from %s",
						len(candidates), cutoff.UTC().Format(time.RFC3339), dir),
				}
				if err := rt.Confirmer().Confirm(req); err != nil {
					return nil, err
				}
			}
			entries := candidates
			if !dry {
				entries, err = store.Prune(f, false)
				if err != nil {
					return nil, apierr.Wrap(err, apierr.CodeInternal,
						"pruning stopped after %d file(s): %v", len(entries), err)
				}
			}
			data := backupsPruneData{Dir: dir, Cutoff: cutoff.UTC(), DryRun: dry, Sample: []backup.Entry{}}
			for i, e := range entries {
				data.Bytes += e.Bytes
				if i < pruneSample {
					data.Sample = append(data.Sample, e)
				}
			}
			data.Truncated = len(entries) > len(data.Sample)
			if dry {
				data.WouldRemove = len(entries)
			} else {
				data.Removed = len(entries)
			}
			return output.New("backups prune", output.KindOpResult, jsonValue(data)), nil
		}),
	}
	fl := cmd.Flags()
	fl.StringVar(&olderThan, "older-than", "", "delete backups older than this (30d, 12h, 2026-01-31, RFC3339)")
	fl.StringVar(&collection, "collection", "", "only this collection's backups")
	fl.StringVar(&global, "global", "", "only this global's backups")
	fl.StringVar(&id, "id", "", "only this document's backups")
	fl.BoolVar(&allProfiles, "all-profiles", false, "prune every profile under the directory, not just the current one")

	SetHelp(cmd, &Help{
		Synopsis: []string{"pay backups prune --older-than D [--collection C] [--id ID] [--all-profiles] [--dry-run] [--yes]"},
		FlagInfo: map[string]FlagInfo{
			"older-than": {Grammar: "a duration (30d, 12h) or a date (2026-01-31, RFC3339)"},
		},
		Output: OutputSpec{Kind: output.KindOpResult,
			Skeleton: `{"dir":"…","cutoff":"…","dry_run":false,"removed":12,"would_remove":0,"bytes":48213,"sample":[…],"truncated":false}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitValidation, apierr.ExitConfirmationRequired},
		Examples: []Example{
			{Why: "see what would go", Cmd: "pay backups prune --older-than 30d --dry-run"},
			{Why: "delete it", Cmd: "pay backups prune --older-than 30d --yes"},
			{Why: "one document's history only", Cmd: "pay backups prune --older-than 7d --collection pages --id 16 --yes"},
			{Why: "every profile under the directory", Cmd: "pay backups prune --older-than 90d --all-profiles --yes"},
		},
		Mistakes: []Mistake{
			{Wrong: "Running prune without --yes in a script and treating exit 11 as failure.",
				Right: "Deleting backups is L2: preview with --dry-run, then pass --yes."},
			{Wrong: "`--older-than 30` without a unit.",
				Right: "Use 30d (days), 12h, or a date; a bare number is rejected."},
			{Wrong: "Expecting prune to touch other profiles.",
				Right: "It prunes the current profile only unless --all-profiles is passed."},
		},
		SeeAlso: []string{"pay backups list", "pay doctor"},
	})
	return cmd
}
