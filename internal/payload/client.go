// Package payload is PayCLI's Payload CMS REST/GraphQL client.
//
// Everything that talks to a Payload server goes through this package, and
// every *http.Request is built in transport.go (§3.1) so that Accept-Language,
// the User-Agent, the request id and the "Authorization is never logged" rule
// cannot be bypassed by a call site. The package never imports internal/cli or
// cobra, and it never reads a clock or an environment variable of its own —
// Config.Now and Config.Sleep are injected by the caller so retries and date
// arithmetic are deterministic under test.
package payload

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/logging"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

// Auth modes (§5.0). These are the same string values apierr uses.
const (
	AuthModeAPIKey    = apierr.AuthModeAPIKey
	AuthModeJWT       = apierr.AuthModeJWT
	AuthModeAnonymous = apierr.AuthModeAnonymous
)

// AuthCollectionAuto is the *unresolved* auth-collection placeholder §4.2
// gives the config key its default value. It is never a real slug: the header
// `Authorization: auto API-Key <key>` returns HTTP 200 with the anonymous view
// (§7.0), so the client refuses to send it and callers must replace it with the
// slug §7.0's Stage -1 resolved (see WithAuthCollection).
const AuthCollectionAuto = "auto"

// Authorization schemes accepted for jwt mode. PayCLI sends JWT by default
// because Bearer is what many reverse proxies consume and strip (§5.0).
const (
	SchemeJWT    = "JWT"
	SchemeBearer = "Bearer"
)

// Defaults from §6 and §9.1.
const (
	DefaultTimeout       = 30 * time.Second
	DefaultUploadTimeout = 120 * time.Second
	DefaultConcurrency   = 8
	MaxConcurrency       = 32
	DefaultMaxRetries    = 3
	DefaultAcceptLang    = "en"
	// DefaultMaxBodyBytes caps a buffered JSON response. Downloads stream and
	// are not affected.
	DefaultMaxBodyBytes int64 = 64 << 20
)

// Config describes one client. All durations and counts fall back to the §6
// defaults when zero.
type Config struct {
	// BaseURL is scheme://host[:port]; any path component is ignored in
	// favour of APIPath.
	BaseURL string
	// APIPath is Payload's routes.api, e.g. "/api". It may be "" for a
	// project that serves the REST API at the root.
	APIPath string
	// GraphQLPath is the full base-relative GraphQL path, which Payload
	// derives as routes.api + routes.graphQL (§2.8). Empty means APIPath +
	// "/graphql".
	GraphQLPath string

	AuthMode       string
	AuthCollection string
	// Credential is the API key in api-key mode and the JWT in jwt mode. It is
	// never logged, never written to an error and never cached.
	Credential string
	// AuthScheme overrides "JWT" for a project whose auth.jwtOrder excludes it.
	AuthScheme string
	// IdentityVerified reports whether Stage 0 confirmed `user != null`. It
	// drives the §11.5 403 split and nothing else.
	IdentityVerified bool

	// Headers are the profile's extra headers, already interpolated.
	Headers map[string]string
	// AcceptLanguage pins the server's translated strings. "en" is
	// load-bearing (§6) and is the default.
	AcceptLanguage string

	Timeout       time.Duration
	UploadTimeout time.Duration
	MaxRetries    int
	Concurrency   int

	InsecureSkipVerify bool
	UserAgent          string
	MaxBodyBytes       int64

	Logger *slog.Logger

	// Now, Sleep, Rand and RequestID are injected so that §3.1's "only app.go
	// reads the wall clock" rule holds and every retry test is deterministic.
	Now       func() time.Time
	Sleep     func(ctx context.Context, d time.Duration) error
	Rand      func() float64
	RequestID func() string

	// RoundTripper replaces the §6 transport wholesale. Tests use it; nothing
	// else should.
	RoundTripper http.RoundTripper

	// Budget is the shared retry budget for a fan-out (§6.1). Nil means one
	// is created per client.
	Budget *Budget

	// Invalidator receives Level-3 reactive-invalidation signals (§8.4) for
	// requests that opted in with WithReactiveInvalidation. Nil disables the
	// mechanism entirely.
	Invalidator Invalidator
}

// Stats are the per-process counters meta.http_requests, meta.retries and
// meta.bytes are built from.
type Stats struct {
	Requests int64
	Retries  int64
	Bytes    int64
}

// Client is a Payload API client. It is safe for concurrent use.
type Client struct {
	cfg    Config
	base   *url.URL
	httpc  *http.Client
	log    *slog.Logger
	budget *Budget
	// state is shared by every credential clone of this client, so the retry
	// stats and the once-per-process staleness guard survive a re-login.
	state *clientState
}

type clientState struct {
	stats   Stats
	reqIDMu sync.Mutex
	reqSeq  uint32
	// lastRequestID is the X-Request-Id of the most recent attempt, shared by
	// every clone. §6 echoes it in meta.request_id so a user can grep the
	// Payload server log for exactly the request the agent made.
	lastRequestID atomic.Value
	// staleUsed guards §8.4(b): at most one Level-3 re-discovery per process.
	staleUsed atomic.Bool
	// routes is the REST + GraphQL prefix every request is built against. It
	// lives here, in the state every clone shares, rather than in cfg, because
	// §7.2's api_path autodiscovery finishes *after* commands have already
	// taken a *Client handle: a copy-on-write swap would leave those handles
	// pointing at the configured /api on a project whose routes.api is not
	// /api, and every read and write would 404 (§21.1 H1).
	routes atomic.Pointer[routePrefix]
}

// routePrefix is the pair §2.8 nests: Payload derives the GraphQL endpoint as
// routes.api + routes.graphQL, so the two never move independently.
type routePrefix struct {
	api     string
	graphQL string
}

// New validates the configuration and builds the client.
func New(cfg Config) (*Client, error) {
	base, err := normalizeBase(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	cfg.APIPath = NormalizePath(cfg.APIPath)
	if cfg.GraphQLPath == "" {
		cfg.GraphQLPath = cfg.APIPath + "/graphql"
	} else {
		cfg.GraphQLPath = NormalizePath(cfg.GraphQLPath)
	}

	switch cfg.AuthMode {
	case "":
		cfg.AuthMode = AuthModeAnonymous
	case AuthModeAPIKey, AuthModeJWT, AuthModeAnonymous:
	default:
		return nil, apierr.New(apierr.CodeInvalidOption, "unknown auth mode %q", cfg.AuthMode).
			WithDidYouMean(apierr.DidYouMean(cfg.AuthMode, []string{AuthModeAPIKey, AuthModeJWT, AuthModeAnonymous})...).
			WithHint("valid modes: api-key, jwt, anonymous")
	}
	if cfg.AuthMode == AuthModeAPIKey && cfg.AuthCollection == "" {
		return nil, apierr.New(apierr.CodeAuthCollectionUnknown,
			"api-key mode needs the auth collection slug: the header is `Authorization: {collection} API-Key {key}`").
			WithHint("pass --auth-collection SLUG, or leave auth_collection = \"auto\" so discovery resolves it")
	}
	switch cfg.AuthScheme {
	case "":
		cfg.AuthScheme = SchemeJWT
	case SchemeJWT, SchemeBearer:
	default:
		return nil, apierr.New(apierr.CodeInvalidOption, "unknown --auth-header-scheme %q", cfg.AuthScheme).
			WithHint("valid schemes: JWT, Bearer")
	}

	if cfg.AcceptLanguage == "" {
		cfg.AcceptLanguage = DefaultAcceptLang
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.UploadTimeout <= 0 {
		cfg.UploadTimeout = DefaultUploadTimeout
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = DefaultMaxRetries
	}
	switch {
	case cfg.Concurrency <= 0:
		cfg.Concurrency = DefaultConcurrency
	case cfg.Concurrency > MaxConcurrency:
		cfg.Concurrency = MaxConcurrency
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if cfg.Logger == nil {
		cfg.Logger = logging.Discard()
	}
	if cfg.Rand == nil {
		cfg.Rand = cryptoFloat
	}

	c := &Client{
		cfg:    cfg,
		base:   base,
		log:    cfg.Logger,
		budget: cfg.Budget,
		state:  &clientState{},
	}
	c.state.routes.Store(&routePrefix{api: cfg.APIPath, graphQL: cfg.GraphQLPath})
	if c.budget == nil {
		c.budget = NewBudget(cfg.MaxRetries * cfg.Concurrency)
	}
	c.httpc = newHTTPClient(cfg)
	return c, nil
}

// normalizeBase enforces H1's precondition that a base URL is an absolute
// http(s) URL, and strips any userinfo so a credential can never reach a log.
func normalizeBase(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, apierr.New(apierr.CodeBaseURLInvalid, "no base URL is configured").
			WithHint("pass --base-url http://localhost:3000 or run `pay auth login --base-url …`")
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, apierr.Wrap(err, apierr.CodeBaseURLInvalid, "the base URL is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, apierr.New(apierr.CodeBaseURLInvalid, "the base URL must be http:// or https://; got %q", u.Scheme).
			WithHint("example: --base-url http://localhost:3000")
	}
	if u.Host == "" {
		return nil, apierr.New(apierr.CodeBaseURLInvalid, "the base URL has no host")
	}
	u.User = nil // §5.3: userinfo never survives into a request or a log
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u, nil
}

// NormalizePath gives a path a leading slash and no trailing slash. A trailing
// slash is not cosmetic: `GET /api/pages/` answers 308 to `/api/pages`
// (verified), costing an extra round trip on every request.
func NormalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimSuffix(p, "/")
}

// Config returns a copy of the client's configuration. The credential is
// included because callers such as the cache scope need its fingerprint; it is
// never printed.
func (c *Client) Config() Config { return c.cfg }

// BaseURL is the normalised scheme://host[:port].
func (c *Client) BaseURL() string { return c.base.String() }

// APIPath is the resolved REST prefix — the one §7.2's ladder settled on when
// it ran, and the configured one otherwise.
func (c *Client) APIPath() string { return c.routes().api }

// routes returns the prefix pair every request is built against.
func (c *Client) routes() *routePrefix {
	if p := c.state.routes.Load(); p != nil {
		return p
	}
	return &routePrefix{api: c.cfg.APIPath, graphQL: c.cfg.GraphQLPath}
}

// AdoptAPIPath installs the api_path §7.2's autodiscovery probed, on this
// client and on every clone of it, so a handle a command took before discovery
// ran still sends its requests to the prefix the project actually serves.
//
// It is deliberately not copy-on-write, unlike WithAuthCollection: the whole
// point is to reach handles that were already handed out. It is idempotent and
// is called from internal/cli's adoption site once a manifest — fresh or
// cached — has settled the prefix, before any data request is issued.
//
// graphQLPath moves with it (§2.8/§4.2): swapping the REST prefix alone would
// leave GraphQL on the old one. An empty graphQLPath re-derives
// apiPath + "/graphql".
func (c *Client) AdoptAPIPath(apiPath, graphQLPath string) {
	next := &routePrefix{api: NormalizePath(apiPath)}
	if graphQLPath == "" {
		next.graphQL = next.api + "/graphql"
	} else {
		next.graphQL = NormalizePath(graphQLPath)
	}
	c.state.routes.Store(next)
}

// AuthMode is the resolved auth mode.
func (c *Client) AuthMode() string { return c.cfg.AuthMode }

// AuthCollection is the resolved auth-collection slug ("" in anonymous mode,
// and "" while the §7.0 placeholder has not been resolved yet).
func (c *Client) AuthCollection() string { return resolvedAuthCollection(c.cfg.AuthCollection) }

// resolvedAuthCollection maps the unresolved placeholder onto "", so that every
// consumer of the slug takes its "I do not know" branch instead of treating
// "auto" as a collection name.
func resolvedAuthCollection(slug string) string {
	if slug == AuthCollectionAuto {
		return ""
	}
	return slug
}

// LastRequestID is the X-Request-Id PayCLI sent on its most recent attempt, or
// "" when no request has been made yet (§6, meta.request_id).
func (c *Client) LastRequestID() string {
	v, _ := c.state.lastRequestID.Load().(string)
	return v
}

// Concurrency is the resolved worker-pool size.
func (c *Client) Concurrency() int { return c.cfg.Concurrency }

// Budget exposes the shared retry budget so a fan-out can hand the same one to
// every worker (§6.1).
func (c *Client) Budget() *Budget { return c.budget }

// Stats snapshots the counters.
func (c *Client) Stats() Stats {
	return Stats{
		Requests: atomic.LoadInt64(&c.state.stats.Requests),
		Retries:  atomic.LoadInt64(&c.state.stats.Retries),
		Bytes:    atomic.LoadInt64(&c.state.stats.Bytes),
	}
}

// WithAuthCollection returns a shallow copy of the client that sends a
// different auth-collection slug, sharing the same connection pool, retry
// budget and counters. It is how the CLI installs the slug §7.0's Stage -1
// resolved into a client that was built with the "auto" placeholder.
func (c *Client) WithAuthCollection(collection string) *Client {
	clone := *c
	clone.cfg.AuthCollection = collection
	return &clone
}

// WithCredential returns a shallow copy of the client that authenticates with
// a different credential, sharing the same HTTP connection pool and retry
// budget. Used after a jwt re-login (§5.0).
func (c *Client) WithCredential(mode, collection, credential string) *Client {
	clone := *c
	clone.cfg.AuthMode = mode
	clone.cfg.AuthCollection = collection
	clone.cfg.Credential = credential
	return &clone
}

// URLFor renders the absolute URL a request would be sent to. It is used for
// the §6.2 length check and for error messages, where it is redacted first.
func (c *Client) URLFor(req *Request) string {
	var b strings.Builder
	b.WriteString(c.base.Scheme)
	b.WriteString("://")
	b.WriteString(c.base.Host)
	b.WriteString(c.base.Path)
	if !req.Absolute {
		b.WriteString(c.routes().api)
	}
	b.WriteString(NormalizePath(req.Path))
	if req.Query != "" {
		b.WriteString("?")
		b.WriteString(req.Query)
	}
	return b.String()
}

// Request is a protocol-level request description. transport.go turns it into
// the single *http.Request this package is allowed to construct.
type Request struct {
	Method string
	// Path is relative to APIPath unless Absolute is set.
	Path     string
	Absolute bool
	// Query is an already-encoded query string (see internal/payload/query).
	Query string

	Body        []byte
	ContentType string
	Accept      string
	// Header carries request-specific headers. Authorization is never set
	// here; the transport owns it.
	Header http.Header

	// Multipart streams an upload body. Mutually exclusive with Body.
	Multipart *Multipart
	// Sink receives a 2xx body instead of buffering it (pay download).
	Sink io.Writer

	Timeout time.Duration

	// NoAuth suppresses the Authorization header (Stage -1 steps 1 and 3,
	// and the unauthenticated Level-2 schema probe).
	NoAuth bool
	// AuthCollectionOverride sends `Authorization: {slug} API-Key …` for a
	// candidate slug during Stage -1 step 4 without mutating the client.
	AuthCollectionOverride string

	// ReadOnly marks a POST that is semantically a read (a GraphQL query, a
	// method-override GET). GET and HEAD are read-only implicitly.
	ReadOnly bool
	// NoOverride forbids the §6.2 method-override promotion. It is implied
	// for PATCH and DELETE, where a wrong Content-Type makes Payload discard
	// the body and act on every document.
	NoOverride bool

	// KnownRoute says the manifest claims this route exists, which is what
	// turns a 404 `Route not found` into a Level-3 staleness signal (§8.4).
	KnownRoute bool

	// Classify supplies the context §11.2/§11.5 need to fold an error body
	// into one code.
	Classify ClassifyContext
}

// Multipart is a streaming multipart/form-data body (§13).
type Multipart struct {
	// Boundary is generated when empty.
	Boundary string
	// Write streams the parts. It runs on its own goroutine and its error is
	// propagated to the caller of Do.
	Write func(w io.Writer, boundary string) error
}

// Response is what came back. Body is buffered unless the request had a Sink.
type Response struct {
	Status      int
	Header      http.Header
	Body        []byte
	ContentType string
	// Method and URL describe the request that produced it; URL is already
	// redacted (§5.3) and is safe to print.
	Method    string
	URL       string
	RequestID string
	Attempts  int
	Retries   int
	Duration  time.Duration
	Bytes     int64
	// Promoted reports that §6.2's method-override fallback was used.
	Promoted bool
}

// OK reports a 2xx status.
func (r *Response) OK() bool { return r != nil && r.Status >= 200 && r.Status < 300 }

// ---------------------------------------------------------------------------
// Per-request logging (§5.3, §6)
// ---------------------------------------------------------------------------

// logRequest emits the one debug line per HTTP attempt that `-v` / `--log-level
// debug` exists to turn on. Before this, both flags enabled a level at which
// the request path logged nothing at all, so an agent whose --where returned
// nothing had no way to see the URL that was actually sent.
//
// Everything that could carry a secret is redacted before it is formatted: the
// URL by logging.URL -> redact.URL (userinfo and ?api-key=/?token= query
// parameters), and the Authorization header by the `authorization` attribute
// key, which internal/logging's replacer rewrites to §5.3's fixed literal
// `<redacted:fp=…>` — the same 16-hex fingerprint §4.4 mandates everywhere. The
// header value is handed over unmodified ON PURPOSE: pre-redacting it here
// would be fingerprinted a second time by that replacer, and routing it through
// the one mask the spec names is what makes the rule impossible to forget.
func (c *Client) logRequest(req *Request, plan *requestPlan, requestID string,
	attempt int, took time.Duration, resp *Response, err error) {
	if c.log == nil || plan == nil {
		return
	}
	if !c.log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	status, size := 0, int64(0)
	if resp != nil {
		status, size = resp.Status, resp.Bytes
	}
	attrs := []any{
		"method", plan.method,
		logging.URL("url", plan.url),
		"status", status,
		"duration_ms", took.Milliseconds(),
		"attempt", attempt,
		"retries", attempt - 1,
		"bytes", size,
		"request_id", requestID,
		"authorization", c.authorization(req),
	}
	if plan.override != "" {
		attrs = append(attrs, "method_override", plan.override)
	}
	if err != nil {
		// redact.Text, not a bare err.Error(): a transport error quotes the URL.
		attrs = append(attrs, "error", redact.Text(err.Error()))
	}
	c.log.Debug("http request", attrs...)
}

// Do sends a request, applying the §6.1 retry policy and the §6.2 URL-length
// fallback. It returns a non-nil *Response whenever the server answered, and a
// non-nil error whenever the request failed or the status was not 2xx — so a
// discovery probe can inspect resp.Status while a command can just check err.
func (c *Client) Do(ctx context.Context, req *Request) (*Response, error) {
	if req == nil {
		return nil, apierr.New(apierr.CodeInternal, "nil request")
	}
	if req.Method == "" {
		req.Method = http.MethodGet
	}
	plan, err := c.plan(req)
	if err != nil {
		return nil, err
	}

	resp, err := c.send(ctx, req, plan)
	if err != nil {
		return resp, err
	}

	// §8.4 Level-3. Opt-in per request, never ambient: internal/discovery must
	// never set the context flag, because its probes deliberately generate
	// every trigger.
	if sig := c.staleSignal(ctx, req, resp); sig != nil {
		return c.handleStale(ctx, req, plan, resp, *sig)
	}
	if !resp.OK() {
		return resp, c.classify(req, resp)
	}
	return resp, nil
}

// cryptoFloat returns a uniform [0,1) from crypto/rand. math/rand would need a
// seed, and a seed needs a clock.
func cryptoFloat() float64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0.5
	}
	return float64(binary.BigEndian.Uint64(b[:])>>11) / float64(1<<53)
}
