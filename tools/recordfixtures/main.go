// Command recordfixtures re-records testdata/fixtures from a live Payload
// instance.
//
//	go run ./tools/recordfixtures -base http://localhost:3900 -key <API-KEY> -out testdata/fixtures
//	make fixtures            # the same thing, from PAY_TEST_BASE_URL / PAY_TEST_API_KEY
//
// Why a tool and not a shell script of curl calls:
//
//   - Every recording goes through internal/payload's real client, so a fixture
//     is by construction the bytes the CLI would actually have received —
//     including the Accept-Language header, the retry policy and the api_path
//     resolution. A fixture recorded by curl can silently differ from what the
//     program sees.
//   - Every body is passed through internal/redact before it touches the disk.
//     A Payload project with useAPIKey returns the API key in plaintext from
//     GET /{auth}/me (§7.8.3), so "record the response" and "commit the
//     response" are not the same operation, and the difference must not be a
//     human's discipline.
//   - Recording the ERROR fixtures requires deliberately provoking six specific
//     failures. Their exact bodies are §11.2's whole subject, and they are
//     exactly the thing nobody re-derives by hand a year later.
//
// The tool is idempotent: it rewrites every fixture it can record and leaves
// the rest alone, so `git diff testdata/fixtures` is the answer to "has Payload
// changed under us".
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

const (
	filePerm  = 0o644
	userAgent = "pay-recordfixtures/1"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "recordfixtures: %s\n", redact.Text(err.Error()))
		os.Exit(1)
	}
}

type options struct {
	base       string
	apiPath    string
	key        string
	authColl   string
	out        string
	collection string
	global     string
	timeout    time.Duration
	only       string
	verbose    bool
	email      string
	username   string
	password   string
}

func run() error {
	var opt options
	flag.StringVar(&opt.base, "base", os.Getenv("PAY_TEST_BASE_URL"), "Payload base URL")
	flag.StringVar(&opt.apiPath, "api-path", "/api", "Payload routes.api prefix")
	flag.StringVar(&opt.key, "key", os.Getenv("PAY_TEST_API_KEY"), "API key of a user in the auth collection")
	flag.StringVar(&opt.authColl, "auth-collection", "users", "auth collection slug")
	flag.StringVar(&opt.out, "out", "testdata/fixtures", "output directory")
	flag.StringVar(&opt.collection, "collection", "pages", "collection to sample documents and fields from")
	flag.StringVar(&opt.global, "global", "header", "global to sample")
	flag.DurationVar(&opt.timeout, "timeout", 60*time.Second, "total deadline")
	flag.StringVar(&opt.only, "only", "", "record only fixtures whose name contains this substring")
	flag.StringVar(&opt.email, "email", os.Getenv("PAY_TEST_EMAIL"), "email for the users_login.json recording")
	flag.StringVar(&opt.username, "username", os.Getenv("PAY_TEST_USERNAME"), "username for the users_login.json recording")
	flag.StringVar(&opt.password, "password", os.Getenv("PAY_TEST_PASSWORD"), "password for the users_login.json recording")
	flag.BoolVar(&opt.verbose, "v", false, "log every recording")
	flag.Parse()

	if strings.TrimSpace(opt.base) == "" {
		return errors.New("-base is required (or set PAY_TEST_BASE_URL)")
	}
	if strings.TrimSpace(opt.key) == "" {
		return errors.New("-key is required (or set PAY_TEST_API_KEY)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), opt.timeout)
	defer cancel()

	client, err := payload.New(payload.Config{
		BaseURL:        opt.base,
		APIPath:        opt.apiPath,
		AuthMode:       payload.AuthModeAPIKey,
		AuthCollection: opt.authColl,
		Credential:     opt.key,
		AcceptLanguage: "en",
		UserAgent:      userAgent,
		Now:            time.Now,
		MaxRetries:     1,
	})
	if err != nil {
		return err
	}

	r := &recorder{opt: opt, client: client}
	if err := os.MkdirAll(opt.out, 0o755); err != nil {
		return err
	}

	r.recordHTTP(ctx)
	r.recordGraphQL(ctx)
	r.recordSynthetic()
	if err := r.recordDiscovery(ctx); err != nil {
		r.skip("manifest.json", err)
		r.skip("fields_"+opt.collection+".json", err)
	}

	return r.report()
}

// ---------------------------------------------------------------------------

type recorder struct {
	opt     options
	client  *payload.Client
	written []string
	skipped map[string]string
}

func (r *recorder) skip(name string, err error) {
	if r.skipped == nil {
		r.skipped = map[string]string{}
	}
	r.skipped[name] = redact.Text(err.Error())
}

// write redacts, pretty-prints and atomically writes one fixture.
//
// Redaction is not optional and has no flag: a fixture is a committed file, and
// §5.3 admits no exception for "it was only test data".
func (r *recorder) write(name string, body []byte) {
	if r.opt.only != "" && !strings.Contains(name, r.opt.only) {
		return
	}
	clean := redact.JSON(body)

	pretty := clean.Data
	var decoded any
	dec := json.NewDecoder(strings.NewReader(string(clean.Data)))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err == nil {
		var buf strings.Builder
		enc := json.NewEncoder(&buf)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(decoded); err == nil {
			pretty = []byte(buf.String())
		}
	}

	path := filepath.Join(r.opt.out, name)
	if err := fsatomic.Write(path, pretty, filePerm); err != nil {
		r.skip(name, err)
		return
	}
	r.written = append(r.written, name)
	if r.opt.verbose {
		note := ""
		if clean.Changed {
			note = fmt.Sprintf("  (redacted %d value(s): %s)", len(clean.Paths), strings.Join(clean.Paths, ", "))
		}
		fmt.Fprintf(os.Stderr, "  %-38s %6d bytes%s\n", name, len(pretty), note)
	}
}

// do performs one request and returns the body whatever the status: an error
// fixture IS a non-2xx body, so a failed call is a successful recording.
func (r *recorder) do(ctx context.Context, req *payload.Request) ([]byte, int, error) {
	resp, err := r.client.Do(ctx, req)
	if resp != nil {
		return resp.Body, resp.Status, nil
	}
	return nil, 0, err
}

func (r *recorder) record(ctx context.Context, name string, req *payload.Request, wantStatus int) {
	body, status, err := r.do(ctx, req)
	if err != nil {
		r.skip(name, err)
		return
	}
	if wantStatus != 0 && status != wantStatus {
		r.skip(name, fmt.Errorf("got HTTP %d, wanted %d (the instance no longer produces this case)", status, wantStatus))
		return
	}
	if len(body) == 0 {
		r.skip(name, errors.New("empty body"))
		return
	}
	r.write(name, body)
}

// ---------------------------------------------------------------------------
// The REST recordings.
// ---------------------------------------------------------------------------

func (r *recorder) recordHTTP(ctx context.Context) {
	coll := r.opt.collection

	// --- the happy path ---
	r.record(ctx, "access.json", &payload.Request{Method: "GET", Path: "/access"}, 200)
	r.record(ctx, "users_me.json", &payload.Request{
		Method: "GET", Path: "/" + r.opt.authColl + "/me",
	}, 200)
	// The anonymous /me body is the one that proves user:null is a 200, which
	// is §11's single most load-bearing Payload behaviour.
	r.record(ctx, "users_me_anon.json", &payload.Request{
		Method: "GET", Path: "/" + r.opt.authColl + "/me", NoAuth: true,
	}, 200)

	r.record(ctx, "pages_list.json", &payload.Request{
		Method: "GET", Path: "/" + coll, Query: "limit=3&depth=0",
	}, 200)

	if id := r.firstID(ctx, coll); id != "" {
		r.record(ctx, "pages_get.json", &payload.Request{
			Method: "GET", Path: "/" + coll + "/" + id, Query: "depth=0",
		}, 200)
	} else {
		r.skip("pages_get.json", fmt.Errorf("%s has no documents to sample", coll))
	}

	r.record(ctx, "globals_header.json", &payload.Request{
		Method: "GET", Path: "/globals/" + r.opt.global, Query: "depth=0",
	}, 200)

	// Custom routes can shadow /versions and answer 200 with a non-list body
	// (§21 A17), so this fixture is recorded whatever the status: the shape is
	// the point.
	r.record(ctx, "versions_list.json", &payload.Request{
		Method: "GET", Path: "/" + coll + "/versions", Query: "limit=3&depth=0",
	}, 0)

	r.record(ctx, "locale_all.json", &payload.Request{
		Method: "GET", Path: "/" + coll, Query: "limit=1&depth=0&locale=all",
	}, 200)

	// --- the six error shapes (§11.2) ---
	r.record(ctx, "error_validation.json", &payload.Request{
		Method: "POST", Path: "/" + coll, Body: []byte(`{}`), ContentType: "application/json",
	}, 400)

	// The same failure in another language. This is the regression guard that
	// keeps anyone from matching on error.message: the code and the field paths
	// are identical, the prose is not.
	r.record(ctx, "error_validation_de.json", &payload.Request{
		Method: "POST", Path: "/" + coll, Body: []byte(`{}`), ContentType: "application/json",
		Header: headers("Accept-Language", "de"),
	}, 400)

	r.record(ctx, "error_notfound.json", &payload.Request{
		Method: "GET", Path: "/" + coll + "/999999999",
	}, 404)

	r.record(ctx, "error_route.json", &payload.Request{
		Method: "GET", Path: "/definitely-not-a-collection",
	}, 404)

	// payload-migrations has endpoints disabled and answers 501 "Cannot GET"
	// (§21 A19). If the target project does not install it, the fixture is
	// skipped rather than faked.
	r.record(ctx, "error_501.json", &payload.Request{
		Method: "GET", Path: "/payload-migrations",
	}, 501)

	r.record500(ctx, coll)
	r.recordLogin(ctx)
}

// recordLogin records the POST /{auth}/login body.
//
// It needs a password, which is the one input this tool cannot discover, so it
// is skipped rather than faked when none is supplied. The recorded body is then
// redacted like every other: the JWT it contains is exactly what §5.3's
// value-level detector exists for, and a committed token would be a live
// credential in the repository.
func (r *recorder) recordLogin(ctx context.Context) {
	const name = "users_login.json"
	if r.opt.password == "" || (r.opt.email == "" && r.opt.username == "") {
		r.skip(name, errors.New("no -password plus -email/-username given; login was not attempted"))
		return
	}
	payloadBody := map[string]string{"password": r.opt.password}
	if r.opt.email != "" {
		payloadBody["email"] = r.opt.email
	} else {
		payloadBody["username"] = r.opt.username
	}
	body, err := json.Marshal(payloadBody)
	if err != nil {
		r.skip(name, err)
		return
	}
	r.record(ctx, name, &payload.Request{
		Method: "POST", Path: "/" + r.opt.authColl + "/login",
		Body: body, ContentType: "application/json", NoAuth: true,
	}, 200)
}

// record500 provokes the one error shape that cannot be requested directly.
//
// On Postgres, an unsupported operator or a bad enum value produces a bare 500
// "Something went wrong."; on Mongo the same input returns 0 documents. Several
// candidates are tried and the first genuine 500 wins, so the fixture is real
// rather than hand-written.
func (r *recorder) record500(ctx context.Context, coll string) {
	candidates := []struct {
		what  string
		query string
	}{
		{"all operator", "where[id][all][0]=1&limit=1"},
		{"near operator", "where[id][near]=1,2,3&limit=1"},
		{"bad enum", "where[_status][equals]=__paycli_not_a_status__&limit=1"},
		{"bad id cast", "where[id][equals]=__paycli_not_an_id__&limit=1"},
	}
	for _, c := range candidates {
		body, status, err := r.do(ctx, &payload.Request{
			Method: "GET", Path: "/" + coll, Query: c.query,
		})
		if err != nil || status != 500 || len(body) == 0 {
			continue
		}
		r.write("error_500.json", body)
		return
	}
	r.skip("error_500.json",
		errors.New("no probe produced a 500 on this instance (expected on MongoDB, where these inputs return 0 docs)"))
}

func (r *recorder) firstID(ctx context.Context, coll string) string {
	body, status, err := r.do(ctx, &payload.Request{
		Method: "GET", Path: "/" + coll, Query: "limit=1&depth=0",
	})
	if err != nil || status != 200 {
		return ""
	}
	var page struct {
		Docs []map[string]json.RawMessage `json:"docs"`
	}
	if err := json.Unmarshal(body, &page); err != nil || len(page.Docs) == 0 {
		return ""
	}
	raw, ok := page.Docs[0]["id"]
	if !ok {
		return ""
	}
	return strings.Trim(string(raw), `"`)
}

func headers(pairs ...string) map[string][]string {
	h := map[string][]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		h[pairs[i]] = append(h[pairs[i]], pairs[i+1])
	}
	return h
}

// ---------------------------------------------------------------------------
// The GraphQL recordings.
// ---------------------------------------------------------------------------

// stage1Query is §7.3's single round trip: the Access type (which names every
// collection and global), the Query field list, and the mode probe folded in.
const stage1Query = `query PayCLIStage1 {
  acc: __type(name: "Access") { name fields { name type { kind name ofType { kind name } } } }
  q: __type(name: "Query") { fields { name args { name type { kind name ofType { kind name } } } type { kind name ofType { kind name } } } }
  m: __type(name: "Mutation") { fields { name } }
}`

// stage2Query is a representative leaf batch: one collection's object type and
// its where-input, which is where §7.4's field kinds and operators come from.
const stage2Query = `query PayCLIStage2($obj: String!, $where: String!) {
  o: __type(name: $obj) { name kind fields { name type { kind name ofType { kind name ofType { kind name } } } } }
  w: __type(name: $where) { name kind inputFields { name type { kind name ofType { kind name } } } }
}`

// partialErrorsQuery asks for one real field and one that does not exist, which
// is how Payload produces a body carrying BOTH data and errors — the shape an
// agent is most likely to mis-handle.
const partialErrorsQuery = `query PayCLIPartial { __typename __paycli_no_such_field__ }`

func (r *recorder) recordGraphQL(ctx context.Context) {
	r.recordGQL(ctx, "graphql_stage1.json", payload.GraphQLRequest{Query: stage1Query})

	singular := singularOf(r.opt.collection)
	r.recordGQL(ctx, "graphql_stage2.json", payload.GraphQLRequest{
		Query: stage2Query,
		Variables: map[string]any{
			"obj":   singular,
			"where": singular + "_where",
		},
	})

	r.recordGQL(ctx, "graphql_partial_errors.json", payload.GraphQLRequest{Query: partialErrorsQuery})
}

func (r *recorder) recordGQL(ctx context.Context, name string, in payload.GraphQLRequest) {
	res, err := r.client.GraphQL(ctx, in)
	if err != nil && (res == nil || len(res.Raw) == 0) {
		r.skip(name, err)
		return
	}
	r.write(name, res.Raw)
}

// singularOf is Payload's own singular type name for a slug: kebab-case
// segments are title-cased and joined ("crm-contacts" -> "CrmContact").
// It is a heuristic used only to pick a sample type to introspect; the real
// mapping is recovered by discovery from the Access type.
func singularOf(slug string) string {
	parts := strings.FieldsFunc(slug, func(r rune) bool { return r == '-' || r == '_' })
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]))
		rest := p[1:]
		rest = strings.TrimSuffix(rest, "s")
		b.WriteString(rest)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Fixtures that cannot be recorded from a healthy instance.
// ---------------------------------------------------------------------------

// recordSynthetic writes the two GraphQL-refusal fixtures.
//
// A healthy Payload project cannot produce them, and standing up a second
// instance with graphQL disabled just to record two bodies is not worth a CI
// service container. They are the verbatim shapes Payload returns, written down
// once, in the one place a reader will look for them.
func (r *recorder) recordSynthetic() {
	r.write("graphql_disabled.json", []byte(`{
  "errors": [
    {
      "message": "Route not found \"/api/graphql\"",
      "extensions": { "statusCode": 404, "name": "NotFound" }
    }
  ]
}`))

	r.write("graphql_introspection_disabled.json", []byte(`{
  "errors": [
    {
      "message": "GraphQL introspection is not allowed, but the query contained __schema or __type.",
      "extensions": { "code": "GRAPHQL_VALIDATION_FAILED" }
    }
  ]
}`))
}

// ---------------------------------------------------------------------------
// Discovery-derived fixtures.
// ---------------------------------------------------------------------------

// recordDiscovery runs the real §7 pipeline and writes its outputs. These are
// not HTTP bodies: manifest.json and the field shard are PayCLI's own
// artefacts, and recording them from the live instance is what keeps the
// offline tests testing a schema that exists.
func (r *recorder) recordDiscovery(ctx context.Context) error {
	now := time.Now
	generation := cache.NewGeneration(now())

	d, err := discovery.New(discovery.Options{
		Client:         r.client,
		BaseURL:        r.opt.base,
		APIPath:        r.opt.apiPath,
		AuthMode:       payload.AuthModeAPIKey,
		AuthCollection: r.opt.authColl,
		Concurrency:    8,
		Profile:        "fixtures",
		Profiles:       []string{"fixtures"},
		ScopeKey:       "fixtures",
		CLIVersion:     "fixtures",
		Generation:     generation,
		Now:            now,
	})
	if err != nil {
		return err
	}
	res, err := d.Run(ctx)
	if err != nil {
		return err
	}

	manifest, err := json.Marshal(res.Manifest)
	if err != nil {
		return err
	}
	r.write("manifest.json", manifest)

	// The id-type-unknown variant. §7.6 makes id_type tri-state, and §9.3
	// disables the local invalid_id rejection whenever it is unknown — the
	// branch that only this fixture exercises.
	if unknown, err := withUnknownIDType(manifest); err == nil {
		r.write("manifest_id_unknown.json", unknown)
	}

	shardName := cache.ShardName(r.opt.collection, cache.KindCollection)
	if shard, ok := res.Shards[shardName]; ok {
		if data, err := json.Marshal(shard); err == nil {
			r.write("fields_"+r.opt.collection+".json", data)
		}
	} else {
		r.skip("fields_"+r.opt.collection+".json",
			fmt.Errorf("discovery produced no shard %q", shardName))
	}
	return nil
}

// withUnknownIDType rewrites every collection's id_type to "unknown".
func withUnknownIDType(manifest []byte) ([]byte, error) {
	var doc map[string]any
	dec := json.NewDecoder(strings.NewReader(string(manifest)))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	colls, _ := doc["collections"].([]any)
	for _, c := range colls {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		m["id_type"] = discovery.IDTypeUnknown
		m["id_type_source"] = discovery.SourceUnknown
	}
	return json.Marshal(doc)
}

// ---------------------------------------------------------------------------

func (r *recorder) report() error {
	sort.Strings(r.written)
	fmt.Fprintf(os.Stderr, "\nrecorded %d fixture(s) into %s\n", len(r.written), r.opt.out)
	for _, n := range r.written {
		fmt.Fprintf(os.Stderr, "  + %s\n", n)
	}
	if len(r.skipped) == 0 {
		return nil
	}
	names := make([]string, 0, len(r.skipped))
	for n := range r.skipped {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(os.Stderr, "\nskipped %d fixture(s):\n", len(names))
	for _, n := range names {
		fmt.Fprintf(os.Stderr, "  - %-32s %s\n", n, r.skipped[n])
	}
	// A skip is information, not a failure: several fixtures legitimately
	// cannot be produced by every Payload project (no MongoDB 500, no
	// payload-migrations). The exit status stays 0 so `make fixtures` in a
	// pipeline does not break on a project that is merely different.
	return nil
}
