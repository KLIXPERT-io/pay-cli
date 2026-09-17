package cli

import (
	"context"
	"errors"
	"io/fs"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/audit"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

func init() { Register(newAuditCmd) }

// auditTailData is the envelope payload of `pay audit tail`.
//
// It carries the log's path and enabled state alongside the events because the
// most common reason for an empty tail is not "nothing happened" but "auditing
// is off" or "$CONFIG moved" — and an agent that only sees `events: []` cannot
// tell those apart.
type auditTailData struct {
	Path     string        `json:"path"`
	Enabled  bool          `json:"enabled"`
	Rotated  []string      `json:"rotated_generations"`
	Returned int           `json:"returned"`
	Events   []audit.Event `json:"events"`
}

// newAuditCmd builds `pay audit` (§12.7). The log itself is written by every
// L1-L3 command; this is the read side.
func newAuditCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "audit",
		GroupID: GroupAdmin,
		Short:   "Inspect the local write-audit log",
		Long: `Every L1-L3 write PayCLI performs is recorded in $CONFIG/audit.log as JSONL,
before and after the call, so an interrupted destructive operation still leaves
a trace. The log never contains an API key, a JWT, an Authorization header in
any form, or a document body; base URLs and paths are redacted before they are
written.

--no-audit (or PAY_NO_AUDIT=1) disables writing. "pay doctor" reports whether
the log is writable.`,
	}
	cmd.AddCommand(newAuditTailCmd(rt))
	cmd.AddCommand(newAuditPathCmd(rt))
	SetHelp(cmd, &Help{
		Synopsis:  []string{"pay audit tail|path"},
		Output:    OutputSpec{Kind: output.KindOpResult},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitValidation, apierr.ExitConfig},
		Examples: []Example{
			{Why: "what did I change recently?", Cmd: "pay audit tail"},
			{Why: "only the deletes, most recent 100", Cmd: "pay audit tail -n 100 --action delete"},
			{Why: "writes that started but never finished — the interrupted ones", Cmd: "pay audit tail --phase pre"},
			{Why: "where is the log?", Cmd: "pay audit path"},
			{Why: "is it actually being written?", Cmd: "pay audit path --path .writable --output id"},
		},
		Mistakes: []Mistake{
			{Wrong: "Expecting the audit log to hold what changed.",
				Right: "It records WHICH documents a write touched (command, action, collection, ids, where, counts), never a document body and never a credential. Use `pay versions list` for before/after content."},
			{Wrong: "Treating the audit log as a server-side record.",
				Right: "It is local to this machine and this user, written by this CLI. A change made in the admin UI, or by another machine, is not in it."},
			{Wrong: "Assuming every command appears.",
				Right: "Only L1-L3 writes are audited. Reads (find/get/count) and `pay download` are not, so an absent entry does not mean nothing happened."},
			{Wrong: "Reading only the `post` records.",
				Right: "Each write has a `pre` and a `post` record. A `pre` with no matching `post` is the signature of a write that was interrupted mid-flight — exactly what the log exists to surface."},
		},
		SeeAlso: []string{
			"pay audit tail --action delete   # the destructive subset",
			"pay doctor   # reports whether the log is writable",
			"pay versions list <collection> --id <id>   # what the content used to be",
		},
	})
	return cmd
}

func newAuditTailCmd(rt *Runtime) *cobra.Command {
	var (
		n          int
		since      string
		action     string
		command    string
		profile    string
		collection string
		phase      string
		rotated    bool
	)

	cmd := &cobra.Command{
		Use:   "tail",
		Short: "Print the most recent audit records",
		Long: `Print the most recent audit records, oldest first, as a single envelope.

Corrupt lines are skipped rather than failing the command: a truncated final
record is the normal shape of a log that was being written when the process was
killed, which is exactly the case the audit log exists to capture.`,
		Args: maxArgs(0, "pay audit tail [-n N] [--since D] [--action delete]"),
		RunE: Handle(rt, "audit tail", func(ctx context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			return runAuditTail(ctx, rt, auditTailOptions{
				n:          n,
				since:      since,
				action:     action,
				command:    command,
				profile:    profile,
				collection: collection,
				phase:      phase,
				rotated:    rotated,
			})
		}),
	}

	f := cmd.Flags()
	f.IntVarP(&n, "n", "n", audit.DefaultTail, "maximum number of records to return")
	f.StringVar(&since, "since", "", "only records newer than this (7d, 24h, 2026-01-31, RFC3339)")
	f.StringVar(&action, "action", "", "filter by action: "+strings.Join(auditActions, ", "))
	f.StringVar(&command, "command", "", "filter by command name, e.g. \"delete\"")
	f.StringVar(&profile, "profile-filter", "", "filter by the profile the write used")
	f.StringVar(&collection, "collection", "", "filter by collection slug")
	f.StringVar(&phase, "phase", "", "filter by record phase: pre|post")
	f.BoolVar(&rotated, "rotated", false, "include the rotated generations (audit.log.1 ...)")

	SetHelp(cmd, &Help{
		Synopsis: []string{
			"pay audit tail [-n N] [--since D] [--action A] [--phase pre|post]",
			"pay audit tail [--command C] [--collection SLUG] [--profile-filter P] [--rotated]",
		},
		Output: OutputSpec{Kind: output.KindOpResult,
			Skeleton: `{"path":"…","returned":12,"events":[{"phase":"post","command":"delete","action":"delete","collection":"pages","ids":["27"],"affected":1,"ok":true,"time":"…"}]}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal, apierr.ExitValidation, apierr.ExitConfig},
		Examples: []Example{
			{Why: "the default: the most recent records, oldest first", Cmd: "pay audit tail"},
			{Why: "only deletes, 100 of them", Cmd: "pay audit tail -n 100 --action delete"},
			{Why: "one collection over the last week", Cmd: "pay audit tail --since 7d --collection pages"},
			{Why: "writes that started but never finished", Cmd: "pay audit tail --phase pre"},
			{Why: "just the ids a bulk delete touched", Cmd: "pay audit tail --action delete -n 1 --path .events[0].ids"},
			{Why: "reach back into rotated generations too", Cmd: "pay audit tail --since 30d --rotated"},
		},
		Mistakes: []Mistake{
			{Wrong: "`--profile staging` to filter the log by profile.",
				Right: "--profile chooses which profile the CLI runs as. The filter is --profile-filter; they are different flags on purpose."},
			{Wrong: "`--action deleted` (or any other guess at the vocabulary).",
				Right: "The action set is closed and validated locally: " + strings.Join(auditActions, ", ") + ". A wrong value fails with did_you_mean instead of matching nothing."},
			{Wrong: "Treating a truncated last line as a broken log.",
				Right: "Corrupt lines are skipped, not fatal. A half-written final record is the normal shape of a log whose process was killed — the case the log exists to capture."},
			{Wrong: "Expecting older history without --rotated.",
				Right: "Only the live audit.log is read by default; rotated generations (audit.log.1 …) need --rotated."},
		},
		SeeAlso: []string{
			"pay audit path   # where the log is, and whether it is writable",
			"pay versions list <collection> --id <id>   # the content behind a change",
		},
	})

	return cmd
}

// auditActions is the closed set §12.7's Action field can hold. It is listed in
// help and validated, so `--action deleted` fails locally with did_you_mean
// instead of silently matching nothing.
var auditActions = []string{
	safety.ActionCreate, safety.ActionUpdate, safety.ActionDelete, safety.ActionTrash,
	safety.ActionRestore, safety.ActionPublish, safety.ActionUnpublish, safety.ActionUpload,
	safety.ActionRestoreVersion, safety.ActionRaw,
}

type auditTailOptions struct {
	n          int
	since      string
	action     string
	command    string
	profile    string
	collection string
	phase      string
	rotated    bool
}

func runAuditTail(_ context.Context, rt *Runtime, opt auditTailOptions) (*output.Envelope, error) {
	if opt.n < 0 {
		return nil, apierr.New(apierr.CodeInvalidArgs, "-n must not be negative.")
	}
	if err := oneOf("action", opt.action, auditActions); err != nil {
		return nil, err
	}
	if err := oneOf("phase", opt.phase, []string{string(audit.PhasePre), string(audit.PhasePost)}); err != nil {
		return nil, err
	}

	tail := audit.TailOptions{
		N:              opt.n,
		Action:         opt.action,
		Command:        opt.command,
		Profile:        opt.profile,
		Collection:     opt.collection,
		Phase:          audit.Phase(opt.phase),
		IncludeRotated: opt.rotated,
	}
	if opt.since != "" {
		since, err := query.ParseSince(opt.since, rt.Now())
		if err != nil {
			return nil, err
		}
		tail.Since = since
	}

	logger := rt.Audit()
	events, err := logger.Tail(tail)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, apierr.Wrap(err, apierr.CodeInternal,
			"cannot read the audit log at %s: %v", logger.Path(), err).
			WithHint("`pay doctor` reports whether the audit log is readable and writable.")
	}
	if events == nil {
		events = []audit.Event{}
	}

	data := auditTailData{
		Path:     logger.Path(),
		Enabled:  logger.Enabled(),
		Rotated:  logger.Generations(),
		Returned: len(events),
		Events:   events,
	}
	if data.Rotated == nil {
		data.Rotated = []string{}
	}

	env := output.New("audit tail", output.KindOpResult, jsonValue(data))
	if !logger.Enabled() {
		env.AddWarning(output.Warning{
			Code:    "audit_disabled",
			Message: "auditing is disabled for this invocation, so nothing new is being recorded.",
			Hint:    "drop --no-audit and unset PAY_NO_AUDIT to record writes again.",
		})
	}
	if next := auditNext(data); next != nil {
		env.WithNext(next)
	}
	return env, nil
}

// auditNext points an agent at the one follow-up worth making: a `pre` record
// with no matching `post` is an interrupted write, and the document it names is
// the thing to go and verify.
func auditNext(data auditTailData) *output.Next {
	for i := len(data.Events) - 1; i >= 0; i-- {
		e := data.Events[i]
		if e.Phase != audit.PhasePre || e.Collection == "" || len(e.IDs) == 0 {
			continue
		}
		if auditHasPost(data.Events, e) {
			continue
		}
		return &output.Next{
			Reason: output.ReasonVerifyWrite,
			Cmd:    "pay get " + e.Collection + " " + e.IDs[0],
			Args:   map[string]any{"collection": e.Collection, "id": e.IDs[0]},
		}
	}
	return nil
}

func auditHasPost(events []audit.Event, pre audit.Event) bool {
	for _, e := range events {
		if e.Phase != audit.PhasePost {
			continue
		}
		if e.Command == pre.Command && e.Collection == pre.Collection &&
			e.Method == pre.Method && !e.Time.Before(pre.Time) {
			return true
		}
	}
	return false
}

func newAuditPathCmd(rt *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "path",
		Short: "Print the audit log's path",
		Args:  maxArgs(0, "pay audit path"),
		RunE: Handle(rt, "audit path", func(_ context.Context, rt *Runtime, _ []string) (*output.Envelope, error) {
			logger := rt.Audit()
			// Always a list, never null: an agent reading .generations must not
			// have to nil-check before iterating.
			generations := logger.Generations()
			if generations == nil {
				generations = []string{}
			}
			data := map[string]any{
				"path":        logger.Path(),
				"enabled":     logger.Enabled(),
				"writable":    logger.Writable() == nil,
				"generations": generations,
			}
			if err := logger.Writable(); err != nil {
				data["error"] = err.Error()
			}
			// --output raw prints the bare path, so `$(pay audit path
			// --output raw)` is usable in a shell.
			return output.New("audit path", output.KindOpResult, jsonValue(data)).
				WithRawBody([]byte(logger.Path()+"\n"), true), nil
		}),
	}
	SetHelp(cmd, &Help{
		Synopsis: []string{"pay audit path"},
		Output: OutputSpec{Kind: output.KindOpResult,
			Skeleton: `{"path":"/home/u/.config/pay/audit.log","enabled":true,"writable":true,"generations":[]}`},
		ExitCodes: []int{apierr.ExitOK, apierr.ExitInternal},
		Examples: []Example{
			{Why: "where the log lives", Cmd: "pay audit path"},
			{Why: "the bare path, for a shell", Cmd: "pay audit path --output raw"},
			{Why: "is auditing on and working?", Cmd: "pay audit path --path .writable --output id"},
			{Why: "which rotated generations exist", Cmd: "pay audit path --path .generations"},
			{Why: "read the log with your own tools", Cmd: "tail -f \"$(pay audit path --output raw)\""},
		},
		Mistakes: []Mistake{
			{Wrong: "Assuming enabled:true means records are landing.",
				Right: "enabled reflects --no-audit/PAY_NO_AUDIT; writable is the one that says the file can actually be written. Check both, or read the `error` field that appears when it cannot."},
			{Wrong: "Deleting audit.log to reset it.",
				Right: "It is an append-only record of destructive operations and rotates on its own. Removing it destroys the evidence that an interrupted delete ever started."},
			{Wrong: "Parsing the envelope when you wanted the path.",
				Right: "--output raw prints the bare path with no JSON around it, which is what makes `$(pay audit path --output raw)` work."},
		},
		SeeAlso: []string{"pay audit tail", "pay doctor", "pay config paths"},
	})
	return cmd
}
