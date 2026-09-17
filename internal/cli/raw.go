package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/audit"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
)

func init() { Register(newRawCmd) }

// rawMethods is the closed set `pay raw` accepts. Closing it is deliberate: an
// unrecognised verb would otherwise be sent and classified by the server's
// reply, and a mistyped destructive verb is precisely what §12 exists to catch
// before it leaves the machine.
var rawMethods = []string{"GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH", "DELETE"}

// rawOptions is the parsed flag state for one `pay raw` invocation.
type rawOptions struct {
	method      string
	path        string
	queries     []string
	data        string
	dataFile    string
	file        string
	rawBody     bool
	contentType string
	accept      string
	headers     []string
}

// newRawCmd builds `pay raw` (§9.8) — the guarantee that PayCLI is never a
// dead end. Custom collection and global `endpoints`, POST /api/graphql,
// plugin routes, POST /{coll}/access/{id}, GET /{coll}/file/{filename},
// GET /payload-jobs/run and anything a future Payload version adds are all
// reachable through it, with profile auth, retry, redaction, the audit log and
// the §12 risk classification intact.
func newRawCmd(rt *Runtime) *cobra.Command {
	opt := rawOptions{}

	cmd := &cobra.Command{
		Use:     "raw <METHOD> <path>",
		GroupID: GroupOther,
		Short:   "Call any Payload endpoint PayCLI does not model",
		Long: `Call any Payload endpoint directly, with profile auth, retry, redaction,
audit and the write-safety classification applied.

The path is resolved against the profile's api_path unless it starts with "/",
in which case it is sent verbatim against the base URL. On the default api_path
of /api:

  pay raw GET users/me                  ->  /api/users/me
  pay raw POST /api/graphql --data ...   ->  /api/graphql
  pay raw GET /admin/...                 ->  /admin/...

The HTTP method decides the risk level: GET, HEAD and OPTIONS are read-only and
run unprompted; DELETE needs --yes; every other verb is a write and is written
to the audit log before and after the call. --output raw returns Payload's body
verbatim, after redaction unless --no-redact.`,
		Args: exactArgs(2, "pay raw <GET|POST|PATCH|PUT|DELETE|HEAD|OPTIONS> <path>"),
		RunE: Handle(rt, "raw", func(ctx context.Context, rt *Runtime, args []string) (*output.Envelope, error) {
			opt.method = args[0]
			opt.path = args[1]
			return runRaw(ctx, rt, opt)
		}),
	}

	f := cmd.Flags()
	f.StringArrayVar(&opt.queries, "query", nil, "query parameter as k=v (repeatable; the value is sent verbatim)")
	f.StringVar(&opt.data, "data", "", "request body as JSON, or @- to read it from stdin")
	f.StringVar(&opt.dataFile, "data-file", "", "read the request body from a file, or - for stdin")
	f.StringVar(&opt.file, "file", "", "send multipart/form-data with this file as the `file` part")
	f.BoolVar(&opt.rawBody, "raw-body", false, "send the body verbatim without validating it as JSON")
	f.StringVar(&opt.contentType, "content-type", "", "override the request Content-Type")
	f.StringVar(&opt.accept, "accept", "", "override the request Accept header")
	f.StringArrayVar(&opt.headers, "header", nil, "extra request header as 'Name: value' (repeatable)")

	SetHelp(cmd, &Help{
		Synopsis: []string{
			"pay raw <GET|HEAD|OPTIONS> <path> [--query k=v]…",
			"pay raw <POST|PATCH|PUT> <path> --data JSON|@- [--file F] [--yes]",
			"pay raw DELETE <path> --yes",
		},
		Args: []ArgSpec{
			{Name: "METHOD", Required: true, Type: "string",
				Values:  []string{"GET", "HEAD", "OPTIONS", "POST", "PATCH", "PUT", "DELETE"},
				Example: "GET"},
			{Name: "path", Required: true, Type: "string",
				Example: "pages  (api_path-relative) — a leading / is sent verbatim: /api/pages, /admin/…"},
		},
		Output: OutputSpec{Kind: output.KindRaw,
			Skeleton: `data is the server's decoded body verbatim — data_kind "raw", no target and no page block. --output raw prints the undecoded bytes instead.`},
		ExitCodes: DestructiveExitCodes,
		Examples: []Example{
			{Why: "who am I, as the server sees me",
				Cmd: "pay raw GET users/me"},
			{Why: "a filter PayCLI does not model; --query values are sent verbatim",
				Cmd: "pay raw GET pages --query 'where[slug][equals]=home' --query limit=1"},
			{Why: "GraphQL, which needs the absolute path",
				Cmd: `pay raw POST /api/graphql --data '{"query":"{ Pages { totalDocs } }"}'`},
			{Why: "Payload's body verbatim, with no envelope around it",
				Cmd: "pay raw GET pages --query limit=1 --output raw"},
			{Why: "a multipart upload to a route PayCLI does not model",
				Cmd: `pay raw POST media --file ./logo.png --data '{"alt":"Logo"}'`},
			{Why: "a write: audited before and after, and DELETE needs --yes",
				Cmd: "pay raw DELETE pages/42 --yes"},
		},
		Mistakes: []Mistake{
			{Wrong: "`pay raw GET /pages` — a leading slash with no api prefix.",
				Right: "A path starting with / is sent VERBATIM against the base URL, so that one reaches /pages and 404s with HTML. Write `pay raw GET pages` (resolved against api_path) or `pay raw GET /api/pages`. The verbatim rule is what lets `pay raw GET /admin/…` reach non-API routes, so it is deliberate."},
			{Wrong: "Expecting --query values to be escaped for you.",
				Right: "They are sent exactly as typed, which is the point: `--query 'where[x][contains]=a%25b'` is yours to encode. PayCLI's own --where does the escaping; raw does not."},
			{Wrong: "Using `pay raw` to avoid a confirmation.",
				Right: "The method sets the risk level: DELETE is L2 and still needs --yes, and every non-GET is written to the audit log. raw bypasses PayCLI's VALIDATION, not its safety gates."},
			{Wrong: "Expecting the usual target/page/changed blocks around a raw result.",
				Right: "data_kind is \"raw\" and data IS the server's decoded body, with no target, page or changed block — PayCLI did not model the route, so it has nothing to say about it. Use --output raw for the undecoded bytes."},
			{Wrong: "Assuming raw follows a custom routes.api prefix.",
				Right: "raw runs no discovery: it resolves against the profile's configured api_path only. On a project that moved routes.api, pass the absolute path or pin api_path with `pay config set profiles.<p>.api_path /cms-api`."},
		},
		Notes: []string{
			"raw validates nothing, but it does REMEMBER (§7.11): a --query whose single " +
				"operator makes the server answer 500 is recorded against that collection in the " +
				"scope cache, and the next raw call using it carries " +
				"warning{code:\"operator_previously_failed\"} with the recorded evidence. " +
				"It is a warning, never a block — `pay cache clear` forgets it.",
		},
		SeeAlso: []string{
			"pay find <collection> --where …   # the modelled, validated form",
			"pay explain --section connection   # the api_path in effect",
			"pay audit tail   # what raw already changed",
		},
	})

	return cmd
}

func runRaw(ctx context.Context, rt *Runtime, opt rawOptions) (*output.Envelope, error) {
	if err := rt.requireServer(); err != nil {
		return nil, err
	}
	ctx, cancel := rt.deadlineContext(ctx)
	defer cancel()

	method, err := normalizeRawMethod(opt.method)
	if err != nil {
		return nil, err
	}
	apiPath := rt.Cfg.APIPath
	reqPath, absolute := resolveRawPath(opt.path)

	query, err := buildRawQuery(opt.queries)
	if err != nil {
		return nil, err
	}
	header, err := buildRawHeaders(opt.headers)
	if err != nil {
		return nil, err
	}

	body, multi, ctype, err := buildRawBody(rt, opt)
	if err != nil {
		return nil, err
	}
	defer multi.close()

	req := &payload.Request{
		Method:      method,
		Path:        reqPath,
		Absolute:    absolute,
		Query:       query,
		Body:        body,
		ContentType: ctype,
		Accept:      opt.accept,
		Header:      header,
		// §6.2's method-override promotion rewrites a GET into a form-encoded
		// POST. `pay raw` is a verbatim channel by definition, so it never
		// opts in: the request an agent reads back from --dry-run must be the
		// request that was sent.
		NoOverride: true,
		Classify: payload.ClassifyContext{
			IncludeRaw: !rt.NoRaw(),
			NoRedact:   !rt.Cfg.Redact,
		},
	}
	if multi != nil {
		req.Multipart = multi.part
	}
	if isReadMethod(method) {
		req.ReadOnly = true
	}

	client, err := rt.Client(ctx)
	if err != nil {
		return nil, err
	}

	op := safety.Op{Command: safety.CmdRaw, Method: method}
	reqURL := client.URLFor(req)

	// §7.11's reactive memo, read half: this operator already failed on this
	// collection against THIS project, so say so before sending rather than
	// after. `pay raw` validates nothing by design, which is exactly why the
	// learned evidence is worth surfacing here — it is the only thing PayCLI
	// knows about a query it is otherwise passing through untouched.
	slug := rawCollectionSlug(reqPath, apiPath, absolute)
	operators := whereOperatorsInQuery(query)
	rt.Warn(rt.learnedOperatorWarnings(slug, operators)...)

	if rt.Cfg.DryRun {
		result := safety.NewDryRun(safety.DryRunRequest{
			Method: method,
			URL:    reqURL,
			Body:   rawBodyPreview(body, multi),
		}, rawWouldAffect(method), nil, !rt.Cfg.Redact)
		env := result.Envelope("raw", rt.Meta())
		env.Data = jsonValue(env.Data)
		return env, nil
	}

	if err := rt.Confirmer().Confirm(safety.Request{
		Op:       op,
		Target:   opt.path,
		Affected: rawWouldAffect(method),
		Summary:  fmt.Sprintf("%s %s", method, reqURL),
	}); err != nil {
		return nil, err
	}

	event := audit.Event{
		Command: "raw",
		Profile: rt.Cfg.Profile,
		BaseURL: rt.Cfg.BaseURL,
		Action:  op.Action(),
		Method:  method,
		Path:    reqURL,
	}
	audited := op.Level().Audited()
	if audited {
		if err := rt.Audit().Pre(event); err != nil {
			return nil, err
		}
	}

	started := rt.Now()
	resp, callErr := client.Do(ctx, req)
	elapsed := rt.Now().Sub(started)

	if audited {
		post := event
		post.DurationMS = elapsed.Milliseconds()
		post.OK = callErr == nil
		if resp != nil {
			post.Status = resp.Status
			post.RequestID = resp.RequestID
			if callErr == nil && !isReadMethod(method) {
				post.Affected = 1
			}
		}
		if callErr != nil {
			post.Err = string(apierr.CodeOf(callErr))
		}
		if w := rt.Audit().Post(post); w != nil {
			rt.Warn(*w)
		}
	}

	// §7.11's reactive memo, write half. The attribution test lives in
	// discovery: a 500 alone proves nothing, so the request must have carried
	// exactly one operator and that operator must be one whose adapter support
	// actually varies. Anything looser and one flaky 502 on `slug eq home`
	// would teach PayCLI that `equals` is broken.
	if failed, evidence := rawServerFailure(resp, callErr); failed {
		if bad, ok := discovery.AttributeOperatorFailure(true, operators); ok {
			rt.rememberOperatorFailure(bad, slug, evidence)
		}
	}

	if callErr != nil {
		return nil, annotateRawError(callErr, opt.path, apiPath)
	}

	rt.Log.Debug("raw call complete",
		slog.String("method", method), slog.Int("status", resp.Status))

	env := output.New("raw", output.KindRaw, decodeRawData(resp.Body)).
		WithRawBody(resp.Body, !rt.Cfg.Redact)
	return env, nil
}

// rawServerFailure reports §7.11's learning trigger — "a 500 or a QueryError" —
// and returns the evidence string the memo records verbatim, in the shape
// §7.11's own example uses ("HTTP 500 Something went wrong.").
//
// A 4xx is deliberately NOT a trigger: a 400 means the server understood the
// operator well enough to reject the VALUE, which is the caller's bug and not
// the adapter's.
func rawServerFailure(resp *payload.Response, callErr error) (bool, string) {
	status := 0
	if resp != nil {
		status = resp.Status
	}
	serverSide := status >= 500
	if !serverSide && apierr.CodeOf(callErr) == apierr.CodeServerError {
		serverSide = true
	}
	if !serverSide {
		return false, ""
	}
	code := strconv.Itoa(status)
	evidence := "HTTP " + code
	if status == 0 {
		evidence = "server error"
	}
	if e := apierr.From(callErr); e != nil && e.Message != "" {
		msg := truncateEvidence(e.Message)
		// Payload's own message usually repeats the status ("Payload returned
		// 500 for GET /api/x"). Recording "HTTP 500 Payload returned 500 …"
		// puts the number twice in a warning an agent has to read.
		if status > 0 && strings.Contains(msg, code) {
			evidence = msg
		} else {
			evidence += " " + msg
		}
	}
	return true, strings.TrimSpace(evidence)
}

// truncateEvidence keeps the memo readable. The evidence is replayed verbatim
// inside a warning message, so an HTML error page must not become the warning.
func truncateEvidence(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const max = 160
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// normalizeRawMethod upper-cases and validates the verb.
func normalizeRawMethod(in string) (string, error) {
	m := strings.ToUpper(strings.TrimSpace(in))
	for _, known := range rawMethods {
		if m == known {
			return m, nil
		}
	}
	return "", apierr.New(apierr.CodeInvalidArgs,
		"%q is not an HTTP method PayCLI will send.", in).
		WithDidYouMean(apierr.DidYouMean(m, rawMethods)...).
		WithHint("Valid methods: %s.", strings.Join(rawMethods, ", "))
}

func isReadMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// rawWouldAffect is the count --dry-run and the confirmation line report. A
// read affects nothing; a write addresses exactly one route, so it is reported
// as one rather than as an unknowable number.
func rawWouldAffect(method string) int {
	if isReadMethod(method) {
		return 0
	}
	return 1
}

// resolveRawPath implements §9.8's rule: the path is relative to api_path
// unless it starts with "/", in which case it is host-absolute. The second
// return value is payload.Request.Absolute.
func resolveRawPath(in string) (string, bool) {
	p := strings.TrimSpace(in)
	if p == "" {
		return "/", false
	}
	if strings.HasPrefix(p, "/") {
		return p, true
	}
	return "/" + p, false
}

// annotateRawError adds the one diagnosis §9.8's path rule makes likely: a
// host-absolute path that forgot the api_path prefix. Without it the most
// common `pay raw` mistake produces a bare route_not_found with no route out.
func annotateRawError(err error, in, apiPath string) error {
	if !strings.HasPrefix(strings.TrimSpace(in), "/") {
		return err
	}
	switch apierr.CodeOf(err) {
	case apierr.CodeRouteNotFound, apierr.CodeDocNotFound:
	case apierr.CodeNonJSONResponse:
		// The common shape of this mistake on a real project: the verbatim
		// path misses the API mount entirely, so the Next.js app answers with
		// an HTML 404 and the generic hint sends the agent to `pay doctor` —
		// which reports every check ok, because base_url and api_path are
		// fine. The path is the problem, so name the retry instead.
	default:
		return err
	}
	prefix := strings.TrimSuffix(apiPath, "/")
	if prefix == "" || in == prefix || strings.HasPrefix(in, prefix+"/") {
		return err
	}
	return apierr.From(err).WithHint(
		"%q starts with / and was therefore sent verbatim, bypassing the profile's api_path %s. "+
			"Drop the leading slash to route it through the API (pay raw ... %s), or spell the full path (%s%s).",
		in, prefix, strings.TrimPrefix(in, "/"), prefix, in)
}

// buildRawQuery turns repeated --query k=v into an encoded query string. Values
// are URL-escaped but never restructured, so bracketed Payload query syntax
// (`where[or][0][and][0][x][equals]=y`) survives verbatim — which is the whole
// reason --where-raw and this flag exist.
func buildRawQuery(pairs []string) (string, error) {
	if len(pairs) == 0 {
		return "", nil
	}
	values := url.Values{}
	for _, raw := range pairs {
		k, v, found := strings.Cut(raw, "=")
		if !found || strings.TrimSpace(k) == "" {
			return "", apierr.New(apierr.CodeInvalidArgs,
				"--query %q is not in k=v form.", raw).
				WithHint("Example: --query 'where[slug][equals]=home' --query limit=1")
		}
		values.Add(k, v)
	}
	// Encode sorts by key, which makes the request reproducible and therefore
	// golden-testable. Payload's bracket parser does not care about order.
	return values.Encode(), nil
}

// buildRawHeaders parses repeated --header 'Name: value'.
func buildRawHeaders(in []string) (http.Header, error) {
	if len(in) == 0 {
		return nil, nil
	}
	h := http.Header{}
	for _, raw := range in {
		name, value, found := strings.Cut(raw, ":")
		name = strings.TrimSpace(name)
		if !found || name == "" {
			return nil, apierr.New(apierr.CodeInvalidArgs,
				"--header %q is not in 'Name: value' form.", raw).
				WithHint("Example: --header 'X-Tenant: acme'")
		}
		h.Add(name, strings.TrimSpace(value))
	}
	return h, nil
}

// rawMultipart carries the streaming body plus the file handle it owns.
type rawMultipart struct {
	part *payload.Multipart
	f    *os.File
}

func (m *rawMultipart) close() {
	if m != nil && m.f != nil {
		_ = m.f.Close()
	}
}

// buildRawBody resolves --data / --data-file / --file into either a buffered
// body or a streaming multipart body.
func buildRawBody(rt *Runtime, opt rawOptions) ([]byte, *rawMultipart, string, error) {
	if opt.data != "" && opt.dataFile != "" {
		return nil, nil, "", apierr.New(apierr.CodeInvalidArgs,
			"--data and --data-file are mutually exclusive.").
			WithHint("Pass the body inline with --data, or name a file with --data-file.")
	}

	var body []byte
	switch {
	case opt.dataFile != "":
		b, err := readMaybeStdin(rt, opt.dataFile)
		if err != nil {
			return nil, nil, "", err
		}
		body = b
	case opt.data == "@-":
		b, err := readMaybeStdin(rt, "-")
		if err != nil {
			return nil, nil, "", err
		}
		body = b
	case opt.data != "":
		body = []byte(opt.data)
	}

	if opt.file != "" {
		if opt.rawBody {
			return nil, nil, "", apierr.New(apierr.CodeInvalidArgs,
				"--raw-body cannot be combined with --file: the body is a multipart envelope.")
		}
		multi, ctype, err := rawFileBody(opt.file, opt.contentType, body)
		if err != nil {
			return nil, nil, "", err
		}
		return nil, multi, ctype, nil
	}

	ctype := opt.contentType
	if len(body) > 0 {
		if !opt.rawBody && !json.Valid(body) {
			return nil, nil, "", apierr.New(apierr.CodeBadRequestBody,
				"the request body is not valid JSON.").
				WithHint("Fix the JSON, or pass --raw-body to send the bytes verbatim " +
					"and --content-type to declare what they are.")
		}
		if ctype == "" {
			ctype = "application/json"
		}
	}
	return body, nil, ctype, nil
}

// readMaybeStdin reads a file, or stdin when the name is "-".
func readMaybeStdin(rt *Runtime, name string) ([]byte, error) {
	if name == "-" {
		in := rt.Stdin()
		if in == nil {
			return nil, apierr.New(apierr.CodeInvalidArgs,
				"no stdin is attached, so the body cannot be read from it.")
		}
		b, err := io.ReadAll(in)
		if err != nil {
			return nil, apierr.Wrap(err, apierr.CodeInvalidArgs,
				"cannot read the request body from stdin: %v", err)
		}
		return b, nil
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeFileMissing, "cannot read %s: %v", name, err)
	}
	return b, nil
}

// rawFileBody builds the streaming multipart body for --file. It reuses §13's
// part names so an upload sent through `pay raw` is byte-identical to one sent
// through `pay upload`.
func rawFileBody(path, contentType string, fields []byte) (*rawMultipart, string, error) {
	if len(fields) > 0 && !json.Valid(fields) {
		return nil, "", apierr.New(apierr.CodeBadRequestBody,
			"--data must be valid JSON when it accompanies --file: it is sent as the %s part.",
			payload.UploadPartFields)
	}
	f, err := os.Open(path) //nolint:gosec // the path is the caller's own argument
	if err != nil {
		return nil, "", apierr.Wrap(err, apierr.CodeFileMissing, "cannot open %s: %v", path, err)
	}
	name := filepath.Base(path)
	ctype := contentType
	if ctype == "" {
		ctype = mime.TypeByExtension(filepath.Ext(name))
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}

	mp := &payload.Multipart{
		Write: func(w io.Writer, boundary string) error {
			mw := multipart.NewWriter(w)
			if boundary != "" {
				if err := mw.SetBoundary(boundary); err != nil {
					return err
				}
			}
			if len(fields) > 0 {
				if err := mw.WriteField(payload.UploadPartFields, string(fields)); err != nil {
					return err
				}
			}
			hdr := textproto.MIMEHeader{}
			hdr.Set("Content-Disposition",
				fmt.Sprintf("form-data; name=%q; filename=%q", payload.UploadPartFile, name))
			hdr.Set("Content-Type", ctype)
			part, err := mw.CreatePart(hdr)
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, f); err != nil {
				return err
			}
			return mw.Close()
		},
	}
	// The transport owns the multipart Content-Type (it has to: the boundary is
	// generated there), so this returns "" rather than fighting it.
	return &rawMultipart{part: mp, f: f}, "", nil
}

// rawBodyPreview is what --dry-run prints as request.body. safety.NewDryRun
// redacts it; a non-JSON body is quoted so the field stays valid JSON.
func rawBodyPreview(body []byte, multi *rawMultipart) json.RawMessage {
	if multi != nil {
		return json.RawMessage(`"<multipart/form-data>"`)
	}
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

// decodeRawData decodes a JSON body into `data` so --path, --output table and
// jq see structure. A non-JSON body leaves data null; the bytes are still
// available through --output raw. Numbers decode through json.Number so a
// 19-digit id never round-trips through float64.
func decodeRawData(body []byte) any {
	if len(body) == 0 || !json.Valid(body) {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	return v
}
