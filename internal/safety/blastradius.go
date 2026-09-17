package safety

import (
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

const (
	// DefaultMaxDocs is defaults.max_bulk (§4.2): the default blast-radius cap
	// on a bulk write. It is NOT a page size — see CheckLimitFlag.
	DefaultMaxDocs = 100
	// ChunkSize is how many ids go into one `?where={"id":{"in":[…]}}` request
	// during §12.3 phase 3.
	ChunkSize = 100
)

// bulkWriteVerbs is the §12.1 L3 row: the verbs on which --where means "an
// unbounded number of documents" and therefore on which --limit is an error.
var bulkWriteVerbs = map[string]bool{
	CmdUpdate: true, CmdDelete: true, CmdPublish: true, CmdUnpublish: true,
}

// IsBulkWriteVerb reports whether --where on this command is a bulk write.
func IsBulkWriteVerb(command string) bool {
	return bulkWriteVerbs[strings.TrimSpace(strings.ToLower(command))]
}

// CheckLimitFlag implements §12.3's flag split: --limit is page size and is
// meaningless on a bulk write, so it is rejected rather than silently
// reinterpreted as a blast-radius cap.
//
// limitSet must be the flag's Changed bit, not "limit != 0" — the default page
// size of 20 is not a user request.
func CheckLimitFlag(command string, selector Selector, limitSet bool) error {
	if !limitSet || selector != SelectorBulk || !IsBulkWriteVerb(command) {
		return nil
	}
	return apierr.New(apierr.CodeInvalidArgs,
		"--limit has no meaning on `pay %s --where`.", strings.TrimSpace(command)).
		WithHint("--limit is page size and has no meaning on a bulk write; use --max-docs N to cap the blast radius, or --all")
}

// ValidateMaxDocs rejects an explicit non-positive --max-docs.
//
// It must be called before anything is sent, on the scoped form as well as the
// bulk one. 0 and negative values cannot be honoured: Plan.Check reads
// MaxDocs <= 0 as "no cap", so passing them through would turn `--max-docs 0`
// into an UNBOUNDED write, while ResolveMaxDocs' fallback silently substitutes
// defaults.max_bulk and then reports a cap the user never typed. An explicit
// blast-radius instruction is never discarded without a word.
func ValidateMaxDocs(flag int, flagSet bool) error {
	if !flagSet || flag > 0 {
		return nil
	}
	return apierr.New(apierr.CodeInvalidArgs,
		"--max-docs must be a positive number; got %d.", flag).
		WithHint("--max-docs N caps how many documents a bulk write may touch and must be >= 1; " +
			"--all is the flag that lifts the cap entirely")
}

// ResolveMaxDocs applies the §4.5 precedence for the blast-radius cap:
// --max-docs, else defaults.max_bulk from config, else DefaultMaxDocs. --all
// lifts the cap entirely and is reported as 0 ("no cap").
//
// A non-positive flag value is NOT a cap and is not silently reinterpreted
// here: callers must have rejected it with ValidateMaxDocs first.
func ResolveMaxDocs(flag int, flagSet bool, configMaxBulk int, all bool) int {
	if all {
		return 0
	}
	if flagSet && flag > 0 {
		return flag
	}
	if configMaxBulk > 0 {
		return configMaxBulk
	}
	return DefaultMaxDocs
}

// Plan is the outcome of §12.3 phases 1 and 2: how many documents match, what
// the cap was, and whether the operation may proceed.
type Plan struct {
	// Command is the bulk verb, used in messages.
	Command string
	// Collection is the target slug.
	Collection string
	// Matched is N from `GET /{coll}/count?where=…`.
	Matched int
	// MaxDocs is the effective cap; 0 means --all was passed.
	MaxDocs int
	// All is true when --all lifted the cap.
	All bool
}

// Check implements §12.3 step 2. It returns bulk_limit_exceeded (exit 5) when
// the match count is over the cap and --all was not passed.
func (p Plan) Check() error {
	if p.All || p.MaxDocs <= 0 || p.Matched <= p.MaxDocs {
		return nil
	}
	verb := strings.TrimSpace(strings.ToLower(p.Command))
	if verb == "" {
		verb = "change"
	}
	where := "--where"
	target := p.Collection
	if target != "" {
		target = " in \"" + target + "\""
	}
	return apierr.New(apierr.CodeBulkLimitExceeded,
		"%d documents%s match %s, which exceeds --max-docs %d.", p.Matched, target, where, p.MaxDocs).
		WithHint("N=%d exceeds --max-docs %d; pass --all to %s everything matching, or narrow --where",
			p.Matched, p.MaxDocs, verb)
}

// Chunk splits resolved ids into ChunkSize-sized batches for §12.3 phase 3.
// The final batch may be short; an empty input yields no batches.
func Chunk(ids []any, size int) [][]any {
	if size <= 0 {
		size = ChunkSize
	}
	if len(ids) == 0 {
		return nil
	}
	out := make([][]any, 0, (len(ids)+size-1)/size)
	for start := 0; start < len(ids); start += size {
		end := start + size
		if end > len(ids) {
			end = len(ids)
		}
		batch := make([]any, end-start)
		copy(batch, ids[start:end])
		out = append(out, batch)
	}
	return out
}

// WhereIDsIn builds the §12.3 phase-3 filter. PayCLI always resolves ids
// client-side and then addresses them explicitly, on PATCH exactly as on
// DELETE, so the blast radius equals what --dry-run printed on both verbs.
func WhereIDsIn(ids []any) map[string]any {
	list := make([]any, len(ids))
	copy(list, ids)
	return map[string]any{"id": map[string]any{"in": list}}
}
