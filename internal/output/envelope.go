// Package output owns PayCLI's single response envelope (§10) and every
// rendering of it.
//
// The envelope is the contract with the agent calling PayCLI: `ok` is the one
// field to branch on, `data_kind` says what shape `data` has so nothing has to
// be guessed, `page.truncated` answers "did I get everything?" without
// arithmetic, and `next.cmd` is a literally runnable string with the current
// flags and profile already baked in.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// SchemaVersion is envelope.v — the integer schema version of the envelope
// itself, bumped only on a breaking change to these keys.
const SchemaVersion = 1

// DataKind names the shape of Envelope.Data (§10.1).
type DataKind string

const (
	KindDoc          DataKind = "doc"
	KindDocList      DataKind = "doc_list"
	KindCount        DataKind = "count"
	KindGlobal       DataKind = "global"
	KindVersion      DataKind = "version"
	KindVersionList  DataKind = "version_list"
	KindBulkResult   DataKind = "bulk_result"
	KindCapabilities DataKind = "capabilities"
	KindSchema       DataKind = "schema"
	KindCommandSpec  DataKind = "command_spec"
	KindOpResult     DataKind = "op_result"
	KindRaw          DataKind = "raw"
	KindError        DataKind = "error"
)

// DataKinds is the closed set, for validation and help.
var DataKinds = []DataKind{
	KindDoc, KindDocList, KindCount, KindGlobal, KindVersion, KindVersionList,
	KindBulkResult, KindCapabilities, KindSchema, KindCommandSpec, KindOpResult,
	KindRaw, KindError,
}

// Valid reports whether k is part of the closed set.
func (k DataKind) Valid() bool {
	for _, known := range DataKinds {
		if k == known {
			return true
		}
	}
	return false
}

// Next reasons (§10.1).
const (
	ReasonMorePages         = "more_pages"
	ReasonRetryFailedSubset = "retry_failed_subset"
	ReasonNeedsAuth         = "needs_auth"
	ReasonVerifyWrite       = "verify_write"
	ReasonDiscoverFirst     = "discover_first"
	ReasonRefreshDiscovery  = "refresh_discovery"
)

// Auth modes reported in meta.auth_mode (§5.0).
const (
	AuthModeAPIKey    = "api-key"
	AuthModeJWT       = "jwt"
	AuthModeAnonymous = "anonymous"
)

// meta.cache.discovery states (§10.1).
const (
	CacheHit         = "hit"
	CacheStaleServed = "stale-served"
	CacheRevalidated = "revalidated"
	CacheMiss        = "miss"
	CacheBypassed    = "bypassed"
	CacheDisabled    = "disabled"
)

// Envelope is the one object PayCLI prints. Struct order IS the JSON key order,
// which is §10.1's listing with `partial` (§12.5), `changed` (§10.2) and
// `error` (§11.1) folded into their natural neighbours: those three sections
// each show a subset and they do not agree on relative position, so the
// success listing in §10.1 wins and the extras sit next to what they describe.
type Envelope struct {
	OK       bool          `json:"ok"`
	V        int           `json:"v"`
	Command  string        `json:"command"`
	DataKind DataKind      `json:"data_kind"`
	Partial  bool          `json:"partial,omitempty"`
	Target   *Target       `json:"target,omitempty"`
	Data     any           `json:"data,omitempty"`
	Changed  *Changed      `json:"changed,omitempty"`
	Edits    *Edits        `json:"edits,omitempty"`
	Page     *Page         `json:"page,omitempty"`
	Error    *apierr.Error `json:"error,omitempty"`
	Next     *Next         `json:"next,omitempty"`
	Meta     Meta          `json:"meta"`
	Warnings []Warning     `json:"warnings"`

	// RawBody is the server's response body for `--output raw`. It never
	// appears in the JSON envelope; the writer emits it instead of the
	// envelope. It is stored already redacted, with RawRedacted recording
	// whether redaction changed the bytes so the writer can print the
	// "--no-redact" notice on stderr (§10.3).
	RawBody     []byte `json:"-"`
	RawRedacted bool   `json:"-"`

	// pathNarrowed records that --path replaced Data with a selection out of
	// it. data_kind still describes what the COMMAND produced (an agent needs
	// that, and it is part of the stable envelope), but the value about to be
	// rendered may no longer be of that shape — which is what the §10.3
	// format matrix is really asking about.
	pathNarrowed bool
}

// Target names what the command acted on.
type Target struct {
	Kind     string `json:"kind"` // "collection" | "global"
	Slug     string `json:"slug"`
	Singular string `json:"singular,omitempty"`
	IDType   string `json:"id_type,omitempty"`
	ID       any    `json:"id,omitempty"`
}

// Page is present iff data_kind == "doc_list" (§10.1). Truncated is the one
// boolean that requires no arithmetic.
type Page struct {
	Limit       int  `json:"limit"`
	Page        int  `json:"page"`
	TotalPages  int  `json:"total_pages"`
	TotalDocs   int  `json:"total_docs"`
	Returned    int  `json:"returned"`
	HasNextPage bool `json:"has_next_page"`
	HasPrevPage bool `json:"has_prev_page"`
	NextPage    *int `json:"next_page"`
	PrevPage    *int `json:"prev_page"`
	Truncated   bool `json:"truncated"`
}

// Changed is the write summary (§10.2). Every counter is always present, even
// at zero, so an agent never has to presence-check one.
type Changed struct {
	Created int   `json:"created"`
	Updated int   `json:"updated"`
	Deleted int   `json:"deleted"`
	Trashed int   `json:"trashed"`
	IDs     []any `json:"ids"`
}

// Edits is the provenance a local transform stamps on its envelope, and the
// only thing that makes `pay get | pay blocks … | pay apply` safe.
//
// Without it `apply` would have to send the whole document back, which means
// PATCHing every field PayCLI happened to read — including the server-owned
// ones (`createdAt`, `_status`) and any relationship the read expanded into an
// object. `Fields` narrows the write to the paths the pipeline actually
// touched, so a pipeline that moved one block writes one key.
//
// It is present only on the local-edit commands and on `apply`; a command that
// talks to Payload never sets it.
type Edits struct {
	// Fields are the document's top-level field paths the pipeline changed, in
	// first-touched order and deduplicated. `apply` builds its PATCH body from
	// exactly these.
	Fields []string `json:"fields"`
	// Ops is every transform applied, oldest first, so the envelope explains
	// itself after four stages of a pipe.
	Ops []EditOp `json:"ops"`
}

// EditOp is one applied transform.
type EditOp struct {
	// Command is the Cmd* constant of the stage that produced it.
	Command string `json:"command"`
	// Field is the document field it edited.
	Field string `json:"field"`
	// Detail is one human-readable sentence: "moved id:a1 (cta) from 0 to 2".
	Detail string `json:"detail"`
	// Matched are the source indices the selector resolved to, recorded
	// because the indices shift under the next stage and this is the only
	// record of what the caller actually addressed.
	Matched []int `json:"matched,omitempty"`
	// Rows is the row count after the op, so a listing is not needed to see
	// that a remove removed something.
	Rows int `json:"rows"`
}

// Touch records a field as edited, keeping Fields in first-touched order and
// free of duplicates.
func (e *Edits) Touch(field string) {
	if e == nil || field == "" {
		return
	}
	for _, f := range e.Fields {
		if f == field {
			return
		}
	}
	e.Fields = append(e.Fields, field)
}

// Next is the follow-up PayCLI recommends. Cmd is literally runnable.
type Next struct {
	Reason       string         `json:"reason"`
	Cmd          string         `json:"cmd"`
	Args         map[string]any `json:"args,omitempty"`
	Alternatives []Alternative  `json:"alternatives,omitempty"`
}

// Alternative is a cheaper or broader way to get the same answer.
type Alternative struct {
	Why string `json:"why"`
	Cmd string `json:"cmd"`
}

// Warning never changes ok or the exit code (§10.1).
type Warning struct {
	Code         string         `json:"code"`
	Message      string         `json:"message"`
	Paths        []string       `json:"paths,omitempty"`
	Sent         map[string]any `json:"sent,omitempty"`
	Returned     map[string]any `json:"returned,omitempty"`
	StillMissing []string       `json:"still_missing,omitempty"`
	Hint         string         `json:"hint,omitempty"`
}

// Warning codes produced by the shared layers. Command-specific codes live
// with their commands.
const (
	WarnRawRedacted              = "raw_redacted"
	WarnDataRedacted             = "data_redacted"
	WarnAnonymousSession         = "anonymous_session"
	WarnIDTypeUnknown            = "id_type_unknown"
	WarnOperatorPreviouslyFailed = "operator_previously_failed"
	WarnInputSilentlyDropped     = "input_silently_dropped"
	WarnValueNormalizedByServer  = "value_normalized_by_server"
	WarnCreatedAsDraft           = "created_as_draft"
)

// Locale carries the RESOLVED values PayCLI actually sent, so an agent can see
// that fallback "none" is why a field came back null. Both are null on a
// non-localised project (§10.1).
type Locale struct {
	Requested *string `json:"requested"`
	Fallback  *string `json:"fallback"`
}

// CacheMeta is meta.cache (§10.1).
type CacheMeta struct {
	Discovery   string `json:"discovery"`
	AgeS        *int64 `json:"age_s"`
	TTLS        *int64 `json:"ttl_s"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Revalidated bool   `json:"revalidated"`
}

// Meta is the per-invocation context. payload_version is null rather than a
// fabricated constant when it could not be determined, and
// payload_version_source always says which it is (§7.11).
type Meta struct {
	RequestID            string  `json:"request_id"`
	CLIVersion           string  `json:"cli_version"`
	Profile              string  `json:"profile"`
	BaseURL              string  `json:"base_url"`
	APIPath              string  `json:"api_path"`
	AuthMode             string  `json:"auth_mode"`
	PayloadVersion       *string `json:"payload_version"`
	PayloadVersionSource string  `json:"payload_version_source"`
	Locale               Locale  `json:"locale"`
	DateField            *string `json:"date_field"`
	// SearchedFields is §9.4's mandatory report for --q: the text fields the
	// OR of `contains` was actually built over. Absent on a command that did
	// not search.
	SearchedFields    []string   `json:"searched_fields,omitempty"`
	Since             *string    `json:"since"`
	Until             *string    `json:"until"`
	DurationMS        int64      `json:"duration_ms"`
	HTTPRequests      int        `json:"http_requests"`
	Retries           int        `json:"retries"`
	Bytes             int64      `json:"bytes"`
	Cache             *CacheMeta `json:"cache,omitempty"`
	DiscoveryRevision string     `json:"discovery_revision,omitempty"`
	DryRun            bool       `json:"dry_run"`
}

// New builds a success envelope. Warnings is always an array, never null.
func New(command string, kind DataKind, data any) *Envelope {
	return &Envelope{
		OK:       true,
		V:        SchemaVersion,
		Command:  command,
		DataKind: kind,
		Data:     data,
		Warnings: []Warning{},
	}
}

// NewError builds the error envelope (§11.1). Any error is accepted; one that
// never passed through apierr becomes `internal`, exit 1.
func NewError(command string, err error) *Envelope {
	return &Envelope{
		OK:       false,
		V:        SchemaVersion,
		Command:  command,
		DataKind: KindError,
		Error:    apierr.From(err),
		Warnings: []Warning{},
	}
}

// WithTarget sets envelope.target.
func (e *Envelope) WithTarget(t *Target) *Envelope {
	e.Target = t
	return e
}

// WithEdits sets envelope.edits — the pipeline provenance a local transform
// carries to the next stage and, finally, to `pay apply`.
func (e *Envelope) WithEdits(ed *Edits) *Envelope {
	e.Edits = ed
	return e
}

// WithPage sets envelope.page. It is only meaningful for doc_list.
func (e *Envelope) WithPage(p *Page) *Envelope {
	e.Page = p
	return e
}

// WithChanged sets the write summary.
func (e *Envelope) WithChanged(c *Changed) *Envelope {
	e.Changed = c
	return e
}

// WithNext sets the recommended follow-up.
func (e *Envelope) WithNext(n *Next) *Envelope {
	e.Next = n
	return e
}

// WithMeta sets the per-invocation context, redacting base_url (§5.3 makes
// redaction of meta.base_url mandatory).
//
// It is also where §5.0's `anonymous_session` warning is attached, because this
// is the ONE funnel meta.auth_mode passes through on its way into an envelope —
// success and error alike. Emitting it here rather than per command is what
// makes "every envelope carries it" true by construction instead of by
// thirty-odd remembered call sites; an anonymous run that silently reports a
// reduced permission matrix as if it were the whole project is a wrong answer,
// not a cosmetic omission.
func (e *Envelope) WithMeta(m Meta) *Envelope {
	m.BaseURL = redact.URL(m.BaseURL)
	e.Meta = m
	e.syncAnonymousWarning()
	return e
}

// syncAnonymousWarning keeps warnings[] in agreement with meta.auth_mode. It is
// idempotent and reversible: WithMeta is called more than once on the same
// envelope (a command sets its own block, then Runtime.emit merges the
// runtime's over it), so the warning is replaced rather than duplicated, and it
// is withdrawn if a later, better-informed meta says the session was
// authenticated after all.
func (e *Envelope) syncAnonymousWarning() {
	idx := -1
	for i := range e.Warnings {
		if e.Warnings[i].Code == WarnAnonymousSession {
			idx = i
			break
		}
	}
	if e.Meta.AuthMode != AuthModeAnonymous {
		if idx >= 0 {
			e.Warnings = append(e.Warnings[:idx], e.Warnings[idx+1:]...)
		}
		return
	}
	// e.Meta.BaseURL is already redacted at this point, so the hint cannot
	// carry an embedded credential (§5.3).
	w := AnonymousSessionWarning(e.Meta.Profile, e.Meta.BaseURL)
	if idx >= 0 {
		e.Warnings[idx] = w
		return
	}
	e.Warnings = append(e.Warnings, w)
}

// AnonymousSessionWarning is §5.0's verbatim notice that no credential was
// resolved. The hint is a literally runnable login command for THIS profile.
func AnonymousSessionWarning(profile, baseURL string) Warning {
	msg := "No credential resolved; running unauthenticated. Permissions, the collection list and field visibility are all reduced."
	if profile != "" {
		msg = fmt.Sprintf("No credential resolved for profile %q; running unauthenticated. Permissions, the collection list and field visibility are all reduced.", profile)
	}
	hint := "pay auth login"
	if profile != "" {
		hint += " --profile " + profile
	}
	if baseURL != "" {
		hint += " --base-url " + baseURL
	}
	return Warning{Code: WarnAnonymousSession, Message: msg, Hint: hint}
}

// IDTypeUnknownWarning is §7.6(a)'s mandatory transparency notice: the
// client-side id check was SKIPPED because PayCLI never learned this
// collection's id type, so a malformed id will come back from the server rather
// than from PayCLI. The hint is the one from §7.6(a) verbatim, so this warning
// and `pay explain limitations` cannot drift apart.
func IDTypeUnknownWarning(slug, profile string) Warning {
	if profile == "" {
		profile = "<profile>"
	}
	return Warning{
		Code: WarnIDTypeUnknown,
		Message: fmt.Sprintf(
			"PayCLI could not determine the id type of %q, so the local id check was skipped and the id was passed to the server unchecked.",
			slug),
		Hint: fmt.Sprintf("pin it once with `pay config set profiles.%s.id_type string`; until then ids are passed through unchecked", profile),
	}
}

// OperatorPreviouslyFailedWarning is §7.11's reactive memo: this operator has
// already failed on this collection, with the recorded evidence, and PayCLI is
// sending it anyway rather than inventing a refusal.
func OperatorPreviouslyFailedWarning(operator, collection, evidence string) Warning {
	msg := fmt.Sprintf("Operator %q previously failed on collection %q; sending it anyway. It may fail again.", operator, collection)
	if evidence != "" {
		msg += " Recorded evidence: " + evidence
	}
	return Warning{
		Code:    WarnOperatorPreviouslyFailed,
		Message: msg,
		Hint:    "If it fails, rewrite the filter without this operator - `pay describe " + collection + " --queryable` lists the operators this collection is known to support.",
	}
}

// AddWarning appends a warning. Warnings never change ok or the exit code.
func (e *Envelope) AddWarning(w Warning) *Envelope {
	e.Warnings = append(e.Warnings, w)
	return e
}

// WithRawBody attaches the server's body for `--output raw`, redacting it
// unless noRedact. When redaction changed the bytes the caller gets
// RawRedacted set, and the writer prints the one-line stderr notice naming
// --no-redact (§10.3).
func (e *Envelope) WithRawBody(body []byte, noRedact bool) *Envelope {
	if noRedact {
		e.RawBody = body
		e.RawRedacted = false
		return e
	}
	res := redact.JSON(body)
	e.RawBody = res.Data
	e.RawRedacted = res.Changed
	return e
}

// RedactData masks every secret value inside .data (§5.3: "scrubs apiKey …
// from ALL output"), returning the paths that changed so the caller can say so
// in warnings[]. It is a no-op when nothing matched, and it is idempotent, so
// a command that already redacted its own payload pays nothing here.
//
// The value is marshalled, spliced by redact.JSON and decoded back with
// UseNumber, which keeps a 19-digit document id and a trailing-zero decimal
// intact. Only a document that actually carried a secret is rebuilt.
func (e *Envelope) RedactData() []string {
	if e == nil || e.Data == nil {
		return nil
	}
	raw, err := marshalCompact(e.Data)
	if err != nil {
		return nil
	}
	res := redact.JSON(raw)
	if !res.Changed {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(res.Data))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil
	}
	e.Data = generic
	return res.Paths
}

// DataRedactedWarning is the §10.3-style notice that .data differs from the
// bytes the server sent, naming the flag that turns the masking off.
func DataRedactedWarning(paths []string) Warning {
	return Warning{
		Code:    WarnDataRedacted,
		Message: "Credential-bearing fields were masked before printing, so .data differs from the server's response.",
		Paths:   paths,
		Hint:    "Re-run with --no-redact to see the true values.",
	}
}

// PropagateRawRedaction turns error.raw's redaction into the warnings[] entry
// §11.1 requires, so the agent knows the bytes differ from the wire.
func (e *Envelope) PropagateRawRedaction() *Envelope {
	if e.Error == nil || len(e.Error.RawRedactedPaths) == 0 {
		return e
	}
	return e.AddWarning(Warning{
		Code:    WarnRawRedacted,
		Message: "error.raw was redacted before it was printed, so its bytes differ from the server's response.",
		Paths:   e.Error.RawRedactedPaths,
		Hint:    "Re-run with --no-redact to see the true bytes, or --no-raw to drop error.raw entirely.",
	})
}

// ExitCode is the process status this envelope implies (§11.4).
func (e *Envelope) ExitCode() int {
	if e == nil {
		return apierr.ExitInternal
	}
	if e.OK {
		return apierr.ExitOK
	}
	return apierr.ExitCode(e.Error)
}

// MarshalIndentTo is the canonical JSON rendering: two-space indent and NO
// HTML escaping. Escaping matters: encoding/json turns "<" into "<" by
// default, which would mangle every "<redacted>" sentinel and every URL
// containing an ampersand in a where clause.
func (e *Envelope) MarshalIndentTo() ([]byte, error) {
	return marshalIndent(e)
}

// Compact renders the envelope on one line, which is what jsonl writes to
// stderr.
func (e *Envelope) Compact() ([]byte, error) {
	return marshalCompact(e)
}
