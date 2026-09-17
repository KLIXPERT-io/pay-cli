package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// Confidence says how the code was decided (§11.1). It is always present.
//
//	certain  — the classification rests on errors[].name, JSON structure or an
//	           HTTP status: signals the project's i18n config cannot change.
//	probable — it rests on a structural heuristic and the confirming English
//	           string was absent. §11.5's file_missing row is the only one.
type Confidence string

const (
	ConfidenceCertain  Confidence = "certain"
	ConfidenceProbable Confidence = "probable"
)

// DefaultDocs is error.docs on every error PayCLI produces.
const DefaultDocs = "pay explain --section exit_codes"

// Field is one entry of error.fields.
type Field struct {
	Path    string `json:"path"`
	Label   string `json:"label,omitempty"`
	Message string `json:"message"`
	// Sent is true iff Path is a leaf of the request body PayCLI actually sent.
	// It exists because Payload validates the WHOLE document on PATCH and
	// therefore reports fields the caller never touched.
	Sent bool `json:"sent"`
}

// HTTP is error.http: what was on the wire when the failure happened.
type HTTP struct {
	Status int    `json:"status"`
	Method string `json:"method"`
	URL    string `json:"url"`
	// PayloadErrorName is errors[0].name verbatim — the signal the normaliser
	// classified on, exposed so an agent can audit the decision.
	PayloadErrorName string `json:"payload_error_name,omitempty"`
	Attempts         int    `json:"attempts"`
	RetryAfter       string `json:"retry_after,omitempty"`
	// BodyExcerpt is at most excerptLimit characters of a non-JSON body.
	BodyExcerpt string `json:"body_excerpt,omitempty"`
}

// Cause is one entry of error.likely_causes, used to diagnose opaque 500s
// (§11.3). Confidence is "high" | "medium" | "low".
type Cause struct {
	Cause      string `json:"cause"`
	Confidence string `json:"confidence"`
	Detail     string `json:"detail,omitempty"`
	Fix        string `json:"fix,omitempty"`
}

// Cause confidence levels.
const (
	CauseHigh   = "high"
	CauseMedium = "medium"
	CauseLow    = "low"
)

// Failure is one entry of error.failures on a partial_failure (§12.5).
type Failure struct {
	ID      any     `json:"id"`
	Code    Code    `json:"code"`
	Message string  `json:"message"`
	Fields  []Field `json:"fields"`
}

// Error is the error half of the envelope (§11.1). Field order is the JSON key
// order: §11.1's listing, with failures and likely_causes inserted after fields
// (§12.5 and §11.3 add them to the same object and the two listings disagree
// on their position; fields-then-failures keeps all the per-item detail
// together).
type Error struct {
	Code         Code            `json:"code"`
	Exit         int             `json:"exit"`
	Message      string          `json:"message"`
	Hint         string          `json:"hint,omitempty"`
	Retriable    bool            `json:"retriable"`
	Confidence   Confidence      `json:"confidence"`
	Fields       []Field         `json:"fields"`
	Failures     []Failure       `json:"failures,omitempty"`
	LikelyCauses []Cause         `json:"likely_causes,omitempty"`
	DidYouMean   []string        `json:"did_you_mean"`
	Docs         string          `json:"docs,omitempty"`
	HTTP         *HTTP           `json:"http,omitempty"`
	Raw          json.RawMessage `json:"raw,omitempty"`

	// RawRedactedPaths is non-empty when redaction modified Raw. The CLI turns
	// it into the warnings[] entry {"code":"raw_redacted","paths":[…]} so the
	// agent knows the bytes differ from the wire (§11.1).
	RawRedactedPaths []string `json:"-"`

	// wrapped is the underlying Go error, for errors.Is/errors.As. It is
	// unexported so it can never reach the envelope.
	wrapped error
}

// New builds an error from a code and a message. The exit status, retriability
// and the default hint all follow from the code, so no caller can invent a
// mapping. The message is scrubbed: server messages routinely embed URLs, and
// a URL can carry a credential.
func New(code Code, format string, args ...any) *Error {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	return &Error{
		Code:       code,
		Exit:       code.Exit(),
		Message:    redact.Text(msg),
		Hint:       DefaultHint(code),
		Retriable:  code.Retriable(),
		Confidence: ConfidenceCertain,
		Fields:     []Field{},
		DidYouMean: []string{},
		Docs:       DefaultDocs,
	}
}

// Wrap attaches a code and message to an existing error, preserving the chain
// for errors.Is / errors.As.
func Wrap(err error, code Code, format string, args ...any) *Error {
	e := New(code, format, args...)
	e.wrapped = err
	return e
}

// Error renders the one-line human form used on stderr (§11.1).
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s (exit %d): %s", e.Code, e.Exit, e.Message)
}

// Unwrap exposes the wrapped cause.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.wrapped
}

// Is lets errors.Is(err, apierr.New(code, "")) match on the code alone.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t != nil && e != nil && t.Code == e.Code
}

// WithHint replaces the default hint. Every "you cannot X" must be followed by
// "do Y instead", so a hint is effectively mandatory.
func (e *Error) WithHint(format string, args ...any) *Error {
	if len(args) > 0 {
		e.Hint = redact.Text(fmt.Sprintf(format, args...))
	} else {
		e.Hint = redact.Text(format)
	}
	return e
}

// WithConfidence overrides the default ConfidenceCertain.
func (e *Error) WithConfidence(c Confidence) *Error {
	e.Confidence = c
	return e
}

// WithFields sets error.fields, sorted sent-first (§11.1).
func (e *Error) WithFields(fields ...Field) *Error {
	e.Fields = SortFields(fields)
	return e
}

// WithFailures sets error.failures for a partial_failure (§12.5).
func (e *Error) WithFailures(failures ...Failure) *Error {
	e.Failures = failures
	return e
}

// WithCauses sets error.likely_causes (§11.3).
func (e *Error) WithCauses(causes ...Cause) *Error {
	e.LikelyCauses = causes
	return e
}

// WithDidYouMean sets error.did_you_mean.
func (e *Error) WithDidYouMean(suggestions ...string) *Error {
	if suggestions == nil {
		suggestions = []string{}
	}
	e.DidYouMean = suggestions
	return e
}

// WithHTTP records the wire context, redacting the URL (§5.3 makes redaction
// of error.http.url mandatory).
func (e *Error) WithHTTP(h *HTTP) *Error {
	if h != nil {
		cp := *h
		cp.URL = redact.URL(cp.URL)
		cp.BodyExcerpt = redact.Text(cp.BodyExcerpt)
		e.HTTP = &cp
	}
	return e
}

// WithRaw attaches the server's body after redaction. When redaction changed
// the bytes, RawRedactedPaths names what moved so the caller can emit the
// raw_redacted warning. noRedact reproduces the true bytes for --no-redact.
func (e *Error) WithRaw(body []byte, noRedact bool) *Error {
	if len(body) == 0 {
		return e
	}
	if noRedact {
		e.Raw = rawJSON(body)
		e.RawRedactedPaths = nil
		return e
	}
	res := redact.JSON(body)
	e.Raw = rawJSON(res.Data)
	if res.Changed {
		e.RawRedactedPaths = res.Paths
	}
	return e
}

// rawJSON makes a server body safe to embed in the envelope. A Payload body is
// JSON, but a misrouted request reaches Next.js and comes back as an HTML error
// page (verified: GET /pages on the live instance), and splicing those bytes
// into a json.RawMessage makes the WHOLE envelope unencodable — an exit-1
// "internal" for an error PayCLI had already classified correctly. A non-JSON
// body is therefore carried as a JSON string instead of being dropped.
func rawJSON(body []byte) json.RawMessage {
	if len(body) == 0 {
		return nil
	}
	if json.Valid(body) {
		return json.RawMessage(body)
	}
	quoted, err := json.Marshal(string(body))
	if err != nil {
		return nil
	}
	return json.RawMessage(quoted)
}

// WithWrapped attaches an underlying Go error after construction.
func (e *Error) WithWrapped(err error) *Error {
	e.wrapped = err
	return e
}

// Recode changes the code, keeping everything else, and re-derives the exit
// status and retriability. It is how a layer with more context sharpens a
// classification — the normaliser cannot tell route_not_found from
// collection_unknown without the manifest, for example.
func (e *Error) Recode(code Code) *Error {
	wasDefault := e.Hint == "" || e.Hint == DefaultHint(e.Code)
	e.Code = code
	e.Exit = code.Exit()
	e.Retriable = code.Retriable()
	if wasDefault {
		e.Hint = DefaultHint(code)
	}
	return e
}

// SortFields orders entries sent-first while preserving the server's order
// inside each group (§11.1).
func SortFields(fields []Field) []Field {
	out := make([]Field, len(fields))
	copy(out, fields)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sent && !out[j].Sent })
	if out == nil {
		return []Field{}
	}
	return out
}

// AnySent reports whether any field was part of the request body PayCLI sent.
func AnySent(fields []Field) bool {
	for _, f := range fields {
		if f.Sent {
			return true
		}
	}
	return false
}

// As finds the first *Error in err's chain.
func As(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) && e != nil {
		return e, true
	}
	return nil, false
}

// CodeOf returns the code of the first *Error in err's chain, or CodeInternal.
func CodeOf(err error) Code {
	if e, ok := As(err); ok {
		return e.Code
	}
	if err == nil {
		return ""
	}
	return CodeInternal
}

// HasCode reports whether err carries the given code.
func HasCode(err error, code Code) bool {
	e, ok := As(err)
	return ok && e.Code == code
}

// From coerces any error into an *Error so the envelope always has one.
// A nil error yields nil.
func From(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := As(err); ok {
		return e
	}
	return Wrap(err, CodeInternal, "%s", err.Error())
}

// defaultHints keeps the "every failure names its remedy" rule enforceable:
// every code has a starting hint, and callers sharpen it with WithHint when
// they know the profile, slug or field involved.
var defaultHints = map[Code]string{
	CodeInternal:                 "This is a bug in PayCLI. Re-run with --log-level debug and open an issue with the request_id from meta.",
	CodeUnknown:                  "PayCLI could not classify this response. error.raw holds the server's body verbatim (after redaction); `pay doctor` checks the endpoint.",
	CodeCacheCorrupt:             "Run `pay cache clear` and retry; the command re-discovers automatically.",
	CodeUpdateVerificationFailed: "The downloaded binary did not match its checksum. Nothing was installed. Re-run `pay self-update`, or install from https://github.com/KLIXPERT-io/pay-cli/releases.",
	CodeAuditWriteFailed:         "The command itself may have succeeded. Check the audit path in `pay config explain`, or set PAY_NO_AUDIT=1 to disable auditing.",

	CodeAuthMissing:             "No credential resolved. Run `pay auth login --profile <p> --base-url <url>`, or set PAY_API_KEY.",
	CodeAuthInvalid:             "The credential was rejected. Verify it with `pay auth status`, then re-run `pay auth login`.",
	CodeAuthRequired:            "You are not authenticated. Run `pay auth login --profile <p>`; anonymous callers see a reduced collection list and reduced fields.",
	CodeAuthLocked:              "The account is locked after too many failed logins. Wait for Payload's lockout window to pass, or have an admin unlock it.",
	CodeAuthUnverifiedEmail:     "The account's email is not verified. Verify it in the Payload admin UI, then retry.",
	CodeAuthInsecurePermissions: "The credentials file is group- or world-readable. Run `pay auth fix-perms`.",
	CodeAuthHelperFailed:        "The profile's credential_helper exited non-zero. Run it by hand to see its stderr, or clear it with `pay config set profiles.<p>.credential_helper ''`.",

	CodeRateLimited: "Retried automatically and still throttled. Lower --concurrency, or wait and retry.",
	CodeDocLocked:   "Another editor holds a lock on this document. Retry shortly, or clear the lock in the admin UI.",
	CodeServerBusy:  "The server asked for a retry later. PayCLI honoured Retry-After and still failed; retry when the server recovers.",

	CodeDocNotFound:     "No document with that id. `pay find <collection> --limit 5` lists real ids.",
	CodeRouteNotFound:   "Payload has no route at that path. `pay collections` lists the slugs this project actually serves.",
	CodeVersionNotFound: "No version with that id. `pay versions <collection> <id>` lists the versions that exist.",

	CodeValidationFailed:    "Fix every path in error.fields and retry. `pay describe <collection> --required-only` lists all required fields with their types. To store an incomplete document instead, add --draft: Payload skips required-field validation for drafts, and `pay publish` will re-run it later.",
	CodeQueryPathInvalid:    "That path cannot be queried. `pay describe <collection>` lists queryable paths; relationship subfields need depth-aware paths like `author.name`.",
	CodeInvalidArgs:         "Check the flag values against `pay <command> --help`.",
	CodeInvalidWhereSyntax:  "--where takes `PATH OP VALUE`, e.g. --where '_status = published'. `pay explain --section where` prints the full operator table.",
	CodeInvalidSortField:    "Payload returns 200 and silently ignores an unknown --sort field, so PayCLI rejects it here. `pay describe <collection>` lists the sortable fields.",
	CodeUnknownField:        "Payload returns 200 and silently drops unknown --select keys, so PayCLI rejects it here. `pay describe <collection>` lists the real field names.",
	CodeInvalidOption:       "Use one of the values listed in error.did_you_mean.",
	CodeInvalidID:           "The id does not match this collection's id type. `pay describe <collection>` reports id_type; pass an id of that type.",
	CodeBadRequestBody:      "The request body was rejected before validation. Check the JSON you passed to --data / --set.",
	CodeWhereRequired:       "Payload requires a `where` on this operation. Add --where, or --all to match every document.",
	CodeFileMissing:         "This is an upload collection: a file is required. Add --file <path>.",
	CodeNotUploadCollection: "This collection does not accept uploads. `pay collections --uploads` lists the ones that do.",
	CodeUnsupportedOperator: "This operator is not supported by the project's database adapter. `pay explain --section where` lists the operators that are.",
	CodeBulkLimitExceeded:   "The match exceeds the configured bulk limit. Narrow --where, or raise the cap with --max-docs N.",
	CodeRequestTooLarge:     "The request body or URL is too large. Use --per-doc for bulk writes, or shrink the payload.",
	CodeFormatUnsupported:   "This command does not support that --output format. The supported ones are named in the message.",
	CodeInvalidPathExpr:     "--path supports exactly three forms: `.a.b` (field access), `.a[0]` (index) and `.a[]` (iterate). This is not jq; pipe the envelope to jq for full expressions.",

	CodeNetworkUnreachable: "The host could not be reached. Check base_url in `pay config explain` and that the server is running.",
	CodeDNSFailure:         "The hostname did not resolve. Check base_url in `pay config explain`.",
	CodeTLSError:           "The TLS handshake failed. For a local self-signed certificate, set insecure_skip_verify in the profile.",
	CodeTimeout:            "The request timed out. Raise --timeout, or narrow the query with --limit / --select.",
	CodeServerError:        "Payload returned 500. It is NOT retried automatically, because a write may already have committed. error.likely_causes ranks the usual causes; `debug: true` in payload.config.ts reveals the real message.",
	CodeServerUnavailable:  "The server or a proxy in front of it is unavailable. Retried automatically and still failing.",
	CodeNonJSONResponse:    "The endpoint returned something that is not JSON. `pay doctor` checks whether base_url + api_path really point at a Payload REST API.",

	CodePartialFailure: "THE SUCCESSFUL WRITES ARE ALREADY COMMITTED. Payload bulk operations are NOT transactional over the REST API - do not re-run this command or you will re-apply it. Retry only the failed ids using next.cmd.",

	CodeAccessDenied:      "Your identity is valid but lacks permission for this operation. `pay can <op> <collection>` shows what this credential may do; ask an admin to widen access control.",
	CodeAdminAccessDenied: "This credential cannot use the admin panel. Use the REST commands instead, or ask an admin to grant admin access.",

	CodeConfigMissing:           "No config file was found. Run `pay config init`, or pass --base-url explicitly.",
	CodeConfigSecretInPlaintext: "An API key is stored in config.toml in plaintext. Move it with `pay auth login --profile <p>`, which writes credentials.json with mode 0600.",
	CodeProfileUnknown:          "`pay profile list` shows the profiles this config defines.",
	CodeBaseURLInvalid:          "base_url must be an absolute http(s) URL, e.g. http://localhost:3000.",
	CodeEndpointNotPayload:      "That base_url + api_path does not serve a Payload REST API. `pay doctor` probes it and reports what it found instead.",
	CodeAuthCollectionUnknown:   "The configured auth collection does not exist on this project. `pay doctor` lists the collections that have auth enabled.",

	CodeCollectionUnknown:    "`pay collections` lists the slugs this project actually serves.",
	CodeGlobalUnknown:        "`pay globals` lists the globals this project actually serves.",
	CodeFeatureUnavailable:   "This project does not have that feature enabled for this collection. `pay describe <collection>` reports its flags.",
	CodeOperationUnsupported: "Payload does not expose that operation on this route. `pay explain --section endpoints` lists what exists.",
	CodeDiscoveryFailed:      "PayCLI could not learn this project's schema. Run `pay discover --refresh`; `pay doctor` reports which discovery stage failed and why.",
	CodeGraphQLDisabled:      "This project does not serve GraphQL, so GraphQL-derived facts are unavailable. PayCLI degrades to REST-only discovery; pin the missing facts in the profile if you need them.",
	CodeSchemaStale:          "The cached schema no longer matches the server. Run `pay discover --refresh`.",

	CodeConfirmationRequired: "Re-run with --yes to confirm, or narrow the blast radius with --where / --max-docs. `--dry-run` shows exactly what would change.",
}

// DefaultHint returns the starting hint for a code.
func DefaultHint(code Code) string { return defaultHints[code] }

// HumanLine renders the one-line stderr summary (§11.1):
//
//	pay: validation_failed (exit 5): 3 fields are invalid on "pages" — hint: …
func HumanLine(e *Error) string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("pay: ")
	b.WriteString(string(e.Code))
	fmt.Fprintf(&b, " (exit %d): ", e.Exit)
	b.WriteString(e.Message)
	if e.Hint != "" {
		b.WriteString(" — hint: ")
		b.WriteString(e.Hint)
	}
	return b.String()
}
