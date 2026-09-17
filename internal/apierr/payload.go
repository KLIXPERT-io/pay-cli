package apierr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Auth modes (§5.0). Duplicated as plain strings rather than imported so that
// apierr stays free of dependencies on config/secret.
const (
	AuthModeAPIKey    = "api-key"
	AuthModeJWT       = "jwt"
	AuthModeAnonymous = "anonymous"
)

// excerptLimit caps error.http.body_excerpt (§11.5).
const excerptLimit = 200

// nextErrorMarker is Next.js's error-boundary marker. Its presence proves the
// framework answered, not Payload.
const nextErrorMarker = `id="__next_error__"`

// Response is everything the normaliser is allowed to look at. It is a plain
// struct rather than an *http.Response so classification is a pure function
// and every row of §11.2 and §11.5 is a table test.
type Response struct {
	Status      int
	Method      string
	URL         string
	ContentType string
	Body        []byte
	Attempts    int
	RetryAfter  string

	// AuthMode and IdentityVerified drive the 403 split. The server cannot make
	// this call for us: a 403 body is byte-identical whether the caller is
	// unauthenticated or merely unpermitted (verified), and its message is
	// translated on top of that.
	AuthMode         string
	IdentityVerified bool

	// UploadCollection is the target collection's flags.upload. A 400 against an
	// upload collection with no errors[0].name is file_missing — a structural,
	// language-independent signal (the English "No files were uploaded." is
	// only a confirming signal that raises confidence).
	UploadCollection bool

	// Collection is the target slug, used in messages only.
	Collection string

	// SentPaths is the leaf-path set of the request body PayCLI actually sent,
	// which decides Field.Sent and therefore which validation hint is used.
	SentPaths map[string]bool

	// IncludeRaw is false for --no-raw; NoRedact is true for --no-redact.
	IncludeRaw bool
	NoRedact   bool
}

// Classify folds any Payload error response into one *Error (§11.2, §11.5).
// It never panics: a project author can make formatErrors return literally
// anything, and afterError hooks can rewrite both body and status.
func Classify(r Response) *Error {
	if e := classifyNonJSON(r); e != nil {
		return finish(e, r, "")
	}

	body, parsed := parseBody(r.Body)
	if !parsed {
		// Rule 6 with no body signals available. An unparseable body is not a
		// reason to throw away a perfectly good status code, but a status that
		// carries no information degrades to "unknown" with raw preserved.
		e := byStatus(r, nil)
		if e == nil {
			e = New(CodeUnknown, "The server returned %d with a body PayCLI could not parse.", r.Status)
		}
		return finish(e, r, "")
	}

	errsRaw, hasErrors := body["errors"]
	errs := toSlice(errsRaw)
	name := firstErrorName(errs)

	// §11.2 rule 1 — docs + a non-empty errors array is a partial failure,
	// whatever the status says. Some documents committed.
	if _, hasDocs := body["docs"]; hasDocs && len(errs) > 0 {
		return finish(classifyPartial(r, body, errs), r, name)
	}

	// §11.2 rule 2 — ValidationError. The NAME is the signal; the message text
	// is carried into raw and the per-field messages, never matched against.
	if name == "ValidationError" {
		if fields, ok := validationFields(errs, r.SentPaths); ok {
			return finish(validationError(r, fields), r, name)
		}
	}

	// §11.2 rule 3 — QueryError. Note the type difference from rule 2:
	// data is an ARRAY here and an object there, so err.data.errors crashes.
	if name == "QueryError" {
		if fields, ok := queryFields(errs); ok {
			e := New(CodeQueryPathInvalid, "%d query path(s) cannot be queried%s.", len(fields), onCollection(r.Collection))
			return finish(e.WithFields(fields...), r, name)
		}
	}

	// §11.2 rule 4 — the Mongoose branch in formatErrors: errors[*] carry a
	// `field` key instead of a ValidationError envelope.
	if fields, ok := mongooseFields(errs, r.SentPaths); ok {
		return finish(validationError(r, fields), r, name)
	}

	// §11.2 rule 5 — a bare message with no errors key.
	if _, hasMessage := body["message"]; hasMessage && !hasErrors {
		switch r.Status {
		case 404:
			// "Route not found \"…\"" is one of Payload's two hardcoded English
			// strings; it is quoted here only to sharpen the message, never to
			// decide the code — the status plus the shape already did that.
			e := New(CodeRouteNotFound, "Payload has no route at %s.", pathOnly(r.URL))
			return finish(e, r, name)
		case 501:
			// "Cannot <METHOD> …" is the other hardcoded English string.
			e := New(CodeOperationUnsupported, "Payload does not support %s on %s.", r.Method, pathOnly(r.URL))
			return finish(e, r, name)
		}
	}

	// §11.2 rule 6 — map by HTTP status.
	if e := byStatus(r, body); e != nil {
		return finish(e, r, name)
	}
	e := New(CodeUnknown, "The server returned %d and PayCLI could not classify the body.", r.Status)
	return finish(e, r, name)
}

// classifyNonJSON implements §11.5's non-JSON rule: an /api/* response that is
// not application/json never reaches json.Unmarshal, because "invalid
// character '<'" is a useless diagnosis.
func classifyNonJSON(r Response) *Error {
	if isJSONContentType(r.ContentType) {
		return nil
	}
	if r.ContentType == "" && len(bytes.TrimSpace(r.Body)) == 0 {
		return nil // no body and no type: let the status decide
	}
	if r.ContentType == "" && looksLikeJSON(r.Body) {
		return nil // no Content-Type but the bytes are JSON; parse them
	}
	msg := fmt.Sprintf("The server returned %s, not JSON (HTTP %d).", describeContentType(r.ContentType), r.Status)
	if bytes.Contains(r.Body, []byte(nextErrorMarker)) {
		msg += " The body carries Next.js's error boundary marker, so the request was answered by Next.js rather than by Payload."
	}
	return New(CodeNonJSONResponse, "%s", msg)
}

func classifyPartial(r Response, body map[string]any, errs []any) *Error {
	succeeded := len(toSlice(body["docs"]))
	total := succeeded + len(errs)
	e := New(CodePartialFailure,
		"Wrote %d of %d documents%s; %d failed.", succeeded, total, onCollection(r.Collection), len(errs))
	e.Failures = partialFailures(errs)
	return e
}

func partialFailures(errs []any) []Failure {
	out := make([]Failure, 0, len(errs))
	for _, raw := range errs {
		m, ok := raw.(map[string]any)
		if !ok {
			out = append(out, Failure{Code: CodeUnknown, Message: toString(raw), Fields: []Field{}})
			continue
		}
		f := Failure{
			ID:      m["id"],
			Code:    CodeValidationFailed,
			Message: toString(m["message"]),
			Fields:  []Field{},
		}
		// isPublic:false means Payload rewrote the message to the generic
		// "Something went wrong." and withheld the real one (§12.5).
		if pub, ok := m["isPublic"].(bool); ok && !pub {
			f.Code = CodeServerError
			f.Message = "The server withheld this document's error message (isPublic: false). Set debug: true in payload.config.ts to reveal it."
		}
		out = append(out, f)
	}
	return out
}

// validationError builds the validation_failed error with §11.1's BRANCHED
// hint. When every reported field is one the caller never sent, telling the
// agent to "fix every path" instructs it to invent a title and a body for
// someone else's document.
func validationError(r Response, fields []Field) *Error {
	sorted := SortFields(fields)
	e := New(CodeValidationFailed, "%d field(s) are invalid%s.", len(sorted), onCollection(r.Collection))
	e.Fields = sorted
	if len(sorted) > 0 && !AnySent(sorted) {
		e.WithHint("The stored document is already invalid on %d field(s) you did not send - Payload validates the whole document on update, so your change was rejected by pre-existing state, not by your input. Retry with --draft to store the change without validation, or repair the listed fields in the same request.", len(sorted))
	}
	return e
}

// byStatus is §11.5's table. It returns nil when the status carries no signal.
func byStatus(r Response, body map[string]any) *Error {
	switch r.Status {
	case 400:
		return classify400(r, body)
	case 401:
		return New(CodeAuthInvalid, "The server rejected the credential (HTTP 401).")
	case 403:
		return classify403(r)
	case 404:
		return New(CodeDocNotFound, "Not found: %s.", pathOnly(r.URL))
	case 413, 414, 431:
		return New(CodeRequestTooLarge, "The request is too large for the server (HTTP %d).", r.Status)
	case 423:
		return New(CodeDocLocked, "The document is locked by another editor (HTTP 423).")
	case 429:
		return New(CodeRateLimited, "Rate limited by the server or a proxy in front of it (HTTP 429).")
	case 500:
		return New(CodeServerError, "Payload returned 500%s. Its message is masked unless the project runs with debug: true.", onRoute(r))
	case 501:
		return New(CodeOperationUnsupported, "Payload does not support %s on %s (HTTP 501).", r.Method, pathOnly(r.URL))
	case 502, 504:
		return New(CodeServerUnavailable, "The server is unavailable (HTTP %d).", r.Status)
	case 503:
		// §11.4 splits 503: with a Retry-After it is a throttle the client can
		// honour (server_busy, exit 3); without one it is an outage (exit 6).
		if strings.TrimSpace(r.RetryAfter) != "" {
			return New(CodeServerBusy, "The server asked PayCLI to retry after %s (HTTP 503).", r.RetryAfter)
		}
		return New(CodeServerUnavailable, "The server is unavailable (HTTP 503).")
	}
	switch {
	case r.Status >= 500:
		return New(CodeServerUnavailable, "The server returned %d.", r.Status)
	case r.Status >= 400:
		// An unmapped 4xx is still the caller's problem, so it exits 5 rather
		// than pretending to be an internal fault.
		return New(CodeBadRequestBody, "The server rejected the request (HTTP %d).", r.Status)
	}
	return nil
}

func classify400(r Response, body map[string]any) *Error {
	name := firstErrorName(toSlice(body["errors"]))
	msg := firstErrorMessage(body)

	// Upload collections: the structural signal is upload + 400 + no
	// errors[0].name. It is adapter- and language-independent. The English
	// string is a CONFIRMING signal only: it raises confidence, and its absence
	// never changes the code.
	if r.UploadCollection && name == "" {
		e := New(CodeFileMissing, "This is an upload collection and the request carried no file (HTTP 400).")
		if strings.Contains(msg, "No files were uploaded") {
			return e
		}
		return e.WithConfidence(ConfidenceProbable)
	}
	if strings.Contains(msg, "Invalid JSON") {
		return New(CodeBadRequestBody, "The server could not parse the request body as JSON (HTTP 400).")
	}
	if strings.Contains(msg, "Missing 'where' query") || strings.Contains(msg, `Missing "where" query`) {
		return New(CodeWhereRequired, "Payload requires a `where` query on this operation (HTTP 400).")
	}
	return New(CodeBadRequestBody, "The server rejected the request body (HTTP 400)%s.", onRouteSuffix(r))
}

// classify403 is the split the server cannot make for us (§11.5). In anonymous
// mode a 403 is ALWAYS exit 2, because "you are not logged in" is the true and
// actionable answer.
func classify403(r Response) *Error {
	if r.AuthMode == AuthModeAnonymous || r.AuthMode == "" || !r.IdentityVerified {
		return New(CodeAuthRequired, "The server refused the request and PayCLI is running %s (HTTP 403).", describeAuthMode(r.AuthMode))
	}
	return New(CodeAccessDenied, "Your identity is valid but is not permitted to perform this operation (HTTP 403).")
}

// finish attaches the wire context and the redacted raw body to every error the
// normaliser produces, so no path can forget them.
func finish(e *Error, r Response, payloadErrorName string) *Error {
	h := &HTTP{
		Status:           r.Status,
		Method:           r.Method,
		URL:              r.URL,
		PayloadErrorName: payloadErrorName,
		Attempts:         max(r.Attempts, 1),
		RetryAfter:       r.RetryAfter,
	}
	if e.Code == CodeNonJSONResponse {
		h.BodyExcerpt = excerpt(r.Body)
	}
	e.WithHTTP(h)
	if r.IncludeRaw {
		e.WithRaw(r.Body, r.NoRedact)
	}
	// Prefer the server's own message when it is present and not one of
	// Payload's generic masks (§11.2): PayCLI's diagnosis becomes the hint.
	if msg := serverMessage(r.Body); msg != "" && !isGenericMessage(msg) && e.Code != CodeNonJSONResponse {
		e.Message = e.Message + " Server said: " + trimTo(msg, excerptLimit)
	}
	return e
}

// --- body helpers (never panic) -----------------------------------------

func parseBody(b []byte) (map[string]any, bool) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	return m, true
}

func toSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// firstErrorName returns errors[0].name, tolerating errors[] entries that are
// strings (formatErrors: `if (Array.isArray(incoming.message)) return {errors:
// incoming.message}` lets a project author return anything).
func firstErrorName(errs []any) string {
	if len(errs) == 0 {
		return ""
	}
	m, ok := errs[0].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := m["name"].(string)
	return name
}

// validationFields reads errors[0].data.errors — an ARRAY inside an OBJECT.
func validationFields(errs []any, sent map[string]bool) ([]Field, bool) {
	if len(errs) == 0 {
		return nil, false
	}
	m, ok := errs[0].(map[string]any)
	if !ok {
		return nil, false
	}
	data, ok := m["data"].(map[string]any)
	if !ok {
		return nil, false
	}
	list, ok := data["errors"].([]any)
	if !ok {
		return nil, false
	}
	out := make([]Field, 0, len(list))
	for _, raw := range list {
		fm, ok := raw.(map[string]any)
		if !ok {
			out = append(out, Field{Path: toString(raw), Message: "This field is invalid."})
			continue
		}
		path := firstString(fm, "path", "field")
		out = append(out, Field{
			Path:    path,
			Label:   toString(fm["label"]),
			Message: toString(fm["message"]),
			Sent:    sent[path],
		})
	}
	return out, true
}

// queryFields reads errors[0].data — an ARRAY, not an object. Code that does
// err.data.errors on a QueryError crashes; this is why the two rules are
// separate.
func queryFields(errs []any) ([]Field, bool) {
	if len(errs) == 0 {
		return nil, false
	}
	m, ok := errs[0].(map[string]any)
	if !ok {
		return nil, false
	}
	list, ok := m["data"].([]any)
	if !ok {
		return nil, false
	}
	out := make([]Field, 0, len(list))
	for _, raw := range list {
		path := toString(raw)
		if fm, ok := raw.(map[string]any); ok {
			path = firstString(fm, "path", "field")
		}
		out = append(out, Field{Path: path, Message: "This path cannot be queried."})
	}
	return out, true
}

// mongooseFields is §11.2 rule 4: errors[*] carrying a `field` key.
func mongooseFields(errs []any, sent map[string]bool) ([]Field, bool) {
	if len(errs) == 0 {
		return nil, false
	}
	out := make([]Field, 0, len(errs))
	for _, raw := range errs {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, false
		}
		field, ok := m["field"].(string)
		if !ok {
			return nil, false
		}
		out = append(out, Field{
			Path:    field,
			Label:   toString(m["label"]),
			Message: toString(m["message"]),
			Sent:    sent[field],
		})
	}
	return out, len(out) > 0
}

func firstErrorMessage(body map[string]any) string {
	if errs := toSlice(body["errors"]); len(errs) > 0 {
		if m, ok := errs[0].(map[string]any); ok {
			if s := toString(m["message"]); s != "" {
				return s
			}
		} else if s := toString(errs[0]); s != "" {
			return s
		}
	}
	return toString(body["message"])
}

// serverMessage extracts the most specific human message the server offered.
func serverMessage(b []byte) string {
	body, ok := parseBody(b)
	if !ok {
		return ""
	}
	return firstErrorMessage(body)
}

// isGenericMessage recognises Payload's masks, which add nothing.
func isGenericMessage(msg string) bool {
	switch strings.TrimSpace(msg) {
	case "Something went wrong.", "Internal Server Error", "Not Found", "Forbidden", "Unauthorized", "":
		return true
	}
	return false
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func toString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func isJSONContentType(ct string) bool {
	mediaType := strings.TrimSpace(strings.ToLower(ct))
	if i := strings.IndexByte(mediaType, ';'); i >= 0 {
		mediaType = strings.TrimSpace(mediaType[:i])
	}
	return mediaType == "application/json" ||
		strings.HasSuffix(mediaType, "+json") ||
		mediaType == "application/graphql-response+json"
}

func looksLikeJSON(b []byte) bool {
	t := bytes.TrimSpace(b)
	return len(t) > 0 && (t[0] == '{' || t[0] == '[')
}

func describeContentType(ct string) string {
	if strings.TrimSpace(ct) == "" {
		return "a body with no Content-Type"
	}
	return ct
}

func describeAuthMode(mode string) string {
	switch mode {
	case AuthModeAPIKey:
		return "with an API key whose identity is not verified"
	case AuthModeJWT:
		return "with a JWT whose identity is not verified"
	default:
		return "anonymously"
	}
}

func excerpt(b []byte) string {
	return trimTo(strings.TrimSpace(string(b)), excerptLimit)
}

func trimTo(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func onCollection(slug string) string {
	if slug == "" {
		return ""
	}
	return fmt.Sprintf(" on %q", slug)
}

func onRoute(r Response) string {
	if p := pathOnly(r.URL); p != "" {
		return " for " + r.Method + " " + p
	}
	return ""
}

func onRouteSuffix(r Response) string {
	if p := pathOnly(r.URL); p != "" {
		return " for " + p
	}
	return ""
}

// pathOnly reduces a URL to its path, which is what a message should name; the
// full URL lives in error.http.url and is redacted there.
func pathOnly(raw string) string {
	if raw == "" {
		return ""
	}
	s := raw
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if j := strings.IndexByte(s, '/'); j >= 0 {
			s = s[j:]
		} else {
			s = "/"
		}
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i]
	}
	return s
}
