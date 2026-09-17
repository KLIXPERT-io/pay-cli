// Package safety implements §12 "Write safety": the four risk levels, the
// confirmation policy, --dry-run and the single blast-radius cap (--max-docs).
//
// Nothing in this package performs I/O against Payload and nothing here reads
// the process environment or the clock — callers inject what they need, so
// every rule below is a table test.
package safety

import "strings"

// Level is a §12.1 risk level.
type Level int

const (
	// L0 read — never prompts.
	L0 Level = iota
	// L1 scoped write — one document, recoverable where versions/trash exist.
	L1
	// L2 scoped destructive — one document, not recoverable.
	L2
	// L3 bulk — an unbounded number of documents.
	L3
)

// String renders the level as it appears in help text and errors.
func (l Level) String() string {
	switch l {
	case L0:
		return "L0"
	case L1:
		return "L1"
	case L2:
		return "L2"
	case L3:
		return "L3"
	}
	return "L?"
}

// Name is the human label from §12.1's table.
func (l Level) Name() string {
	switch l {
	case L0:
		return "read"
	case L1:
		return "scoped write"
	case L2:
		return "scoped destructive"
	case L3:
		return "bulk"
	}
	return "unknown"
}

// Audited reports whether §12.7 requires an audit record. Reads are never
// audited; every write is.
func (l Level) Audited() bool { return l >= L1 }

// Selector says how many documents the operation addresses.
type Selector int

const (
	// SelectorNone — the operation creates a document or targets a global.
	SelectorNone Selector = iota
	// SelectorID — exactly one document, addressed by id.
	SelectorID
	// SelectorBulk — --where / --ids: an unbounded set.
	SelectorBulk
)

// Canonical command names. These are the strings PayCLI uses in the envelope's
// "command" field and in an audit Event, so the same constant identifies an
// operation everywhere.
const (
	CmdFind            = "find"
	CmdGet             = "get"
	CmdCount           = "count"
	CmdDescribe        = "describe"
	CmdCollections     = "collections"
	CmdExplain         = "explain"
	CmdAccess          = "access"
	CmdCan             = "can"
	CmdWhoami          = "whoami"
	CmdDownload        = "download"
	CmdDiscover        = "discover"
	CmdDoctor          = "doctor"
	CmdCache           = "cache"
	CmdRaw             = "raw"
	CmdVersionsList    = "versions list"
	CmdVersionsGet     = "versions get"
	CmdVersionsDiff    = "versions diff"
	CmdGlobalsList     = "globals list"
	CmdGlobalsGet      = "globals get"
	CmdCreate          = "create"
	CmdUpdate          = "update"
	CmdUpload          = "upload"
	CmdDuplicate       = "duplicate"
	CmdPublish         = "publish"
	CmdUnpublish       = "unpublish"
	CmdDelete          = "delete"
	CmdRestore         = "restore"
	CmdGlobalsUpdate   = "globals update"
	CmdVersionsRestore = "versions restore"
)

// readCommands is the §12.1 L0 row. `raw` is L0 only for a GET; the caller
// passes the effective verb through Op.Method for the other methods.
var readCommands = map[string]bool{
	CmdFind: true, CmdGet: true, CmdCount: true, CmdDescribe: true,
	CmdCollections: true, CmdExplain: true, CmdAccess: true, CmdCan: true,
	CmdWhoami: true, CmdDownload: true, CmdDiscover: true, CmdDoctor: true,
	CmdCache: true, CmdVersionsList: true, CmdVersionsGet: true,
	CmdVersionsDiff: true, CmdGlobalsList: true, CmdGlobalsGet: true,
}

// Op is one operation about to be performed, described in the terms §12 cares
// about. It is deliberately data-only: Level, Action and the confirmation
// decision are pure functions of it.
type Op struct {
	// Command is one of the Cmd* constants.
	Command string
	// Selector says how many documents are addressed.
	Selector Selector
	// Permanent is --permanent on delete: the real DELETE rather than the
	// soft-delete PATCH.
	Permanent bool
	// TrashEnabled is the manifest's answer for the target collection. When it
	// is false a delete cannot be undone and is therefore one level riskier
	// (§12.4).
	TrashEnabled bool
	// HardDelete is set when the command puts a raw DELETE on the wire
	// regardless of what trash says — today only `delete --where
	// --unsafe-passthrough-where`, which hands the filter to the server and
	// cannot express a soft delete. Every safety signal has to describe the
	// request that is actually sent, so this makes the operation read as a
	// hard, irreversible delete even on a trash-enabled collection where
	// --permanent was never passed.
	HardDelete bool
	// Method is the HTTP method for `pay raw`, which has no fixed risk level.
	Method string
	// Global is true when the target is a global rather than a collection.
	Global bool
}

// Level classifies the operation per §12.1.
//
// Two rows are not literally in the table and are derived from §12.4 instead:
// `delete <id>` without --permanent on a trash-enabled collection is a
// recoverable PATCH, so it is L1; on a collection without trash the same
// command is irreversible, so it stays L2. `restore` is the inverse of a soft
// delete and is therefore L1.
func (o Op) Level() Level {
	cmd := strings.TrimSpace(strings.ToLower(o.Command))
	if cmd == CmdRaw {
		return rawLevel(o.Method)
	}
	if readCommands[cmd] {
		return L0
	}
	if o.Selector == SelectorBulk {
		// update/delete/publish/unpublish --where. Any other bulk write verb
		// added later lands here too, which is the safe default.
		return L3
	}
	switch cmd {
	case CmdCreate, CmdUpload, CmdDuplicate, CmdUpdate, CmdPublish, CmdRestore:
		return L1
	case CmdUnpublish, CmdGlobalsUpdate, CmdVersionsRestore:
		return L2
	case CmdDelete:
		if o.Permanent || o.HardDelete || !o.TrashEnabled {
			return L2
		}
		return L1
	}
	// An unknown verb is assumed destructive rather than assumed safe.
	return L2
}

// rawLevel classifies `pay raw`, whose risk is entirely in the HTTP method.
func rawLevel(method string) Level {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "", "GET", "HEAD", "OPTIONS":
		return L0
	case "DELETE":
		return L2
	default: // POST, PATCH, PUT
		return L1
	}
}

// Audit Event.Action values (§12.7).
const (
	ActionCreate         = "create"
	ActionUpdate         = "update"
	ActionDelete         = "delete"
	ActionTrash          = "trash"
	ActionRestore        = "restore"
	ActionPublish        = "publish"
	ActionUnpublish      = "unpublish"
	ActionUpload         = "upload"
	ActionRestoreVersion = "restore_version"
	ActionRaw            = "raw"
)

// Action is the audit Event.Action for this operation.
//
// §12.7's comment lists create|update|delete|trash|restore|publish|upload|
// restore_version. "unpublish" is added because folding it into "publish"
// would make the audit log unable to answer "who took this page offline",
// which is the question the log exists for.
func (o Op) Action() string {
	switch strings.TrimSpace(strings.ToLower(o.Command)) {
	case CmdCreate, CmdDuplicate:
		return ActionCreate
	case CmdUpload:
		return ActionUpload
	case CmdUpdate, CmdGlobalsUpdate:
		return ActionUpdate
	case CmdPublish:
		return ActionPublish
	case CmdUnpublish:
		return ActionUnpublish
	case CmdRestore:
		return ActionRestore
	case CmdVersionsRestore:
		return ActionRestoreVersion
	case CmdDelete:
		if o.Permanent || o.HardDelete || !o.TrashEnabled {
			return ActionDelete
		}
		return ActionTrash
	case CmdRaw:
		return ActionRaw
	}
	return strings.TrimSpace(strings.ToLower(o.Command))
}

// SoftDelete reports whether `pay delete` will issue the §12.4 soft-delete
// PATCH rather than a real DELETE.
func (o Op) SoftDelete() bool {
	return strings.EqualFold(strings.TrimSpace(o.Command), CmdDelete) &&
		!o.Permanent && !o.HardDelete && o.TrashEnabled
}

// Irreversible reports whether the operation destroys data with no PayCLI-side
// recovery path. §12.4 requires a stderr warning in exactly this case.
func (o Op) Irreversible() bool {
	cmd := strings.TrimSpace(strings.ToLower(o.Command))
	if cmd != CmdDelete {
		return false
	}
	return o.Permanent || o.HardDelete || !o.TrashEnabled
}

// IrreversibleReason explains WHY Irreversible() is true. The two causes need
// different words: "--permanent bypassed a trash that exists" is a statement
// about this command, while "the collection has no trash" is a claim about the
// project's schema — and printing the second one on a trash-enabled collection
// is simply false, which an agent may act on (concluding `pay restore` does not
// exist there). It returns "" when the operation is reversible.
func (o Op) IrreversibleReason() string {
	if !o.Irreversible() {
		return ""
	}
	switch {
	case o.HardDelete && o.TrashEnabled:
		return "--unsafe-passthrough-where sends a raw DELETE that bypasses this collection's trash, so `pay restore` cannot bring it back"
	case o.Permanent && o.TrashEnabled:
		return "--permanent bypasses this collection's trash, so `pay restore` cannot bring it back"
	}
	return "this collection has no trash, so there is no restore"
}
