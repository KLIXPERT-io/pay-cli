package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/config"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/payload/query"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
	"github.com/KLIXPERT-io/pay-cli/internal/safety"
	"github.com/KLIXPERT-io/pay-cli/internal/secret"
)

// ---------------------------------------------------------------------------
// pay url — where a document lives on the site, and a draft-preview cookie (F9)
// ---------------------------------------------------------------------------

// Path sources, in resolution order (F9).
const (
	PathSourceBreadcrumbs = "breadcrumbs"
	PathSourceTemplate    = "template"
	PathSourceSlug        = "slug"
	PathSourceHome        = "home"
)

// Warning codes `pay url` emits.
const (
	WarnURLPathGuessed = "url_path_guessed"
	WarnURLNotLive     = "url_not_live"
	WarnPreviewNoAuth  = "preview_credential_withheld"
)

// CookieFilePerm is the preview cookie file's mode: the cookie IS draft access
// to the whole site for as long as the build lives.
const CookieFilePerm = 0o600

type urlFlags struct {
	slug       string
	draft      bool
	preview    bool
	cookieFile string
	locale     string
}

func init() { Register(newURLCmd) }

func newURLCmd(rt *Runtime) *cobra.Command {
	f := &urlFlags{}
	cmd := &cobra.Command{
		GroupID: GroupRead,
		Use:     "url <collection> <id> | url <collection> --slug SLUG",
		Short:   "Print a document's frontend URL; --preview also mints a draft-preview cookie",
		Long: `Print where a document lives on the SITE (not the API), and whether that is live.

The path is resolved in this order, and data.path_source says which one answered:
  breadcrumbs  the last breadcrumbs[].url (the nested-docs plugin keeps it current)
  template     the profile's url_paths.<collection>, e.g. "/posts/{slug}"; any
               {field} or {a.b} of the document, {id}, {locale}
  slug         "/{slug}" — the Payload website template's pages route
  home         a path of /home (or slug home) is served at "/"
The host is the profile's site_url, which defaults to base_url's origin because
Payload 3 serves the site and the API from one Next.js app.

--preview performs the site's draft-preview handshake: GET preview_path (default
the website template's /next/preview?path={path}&previewSecret={secret}) with the
profile's credential, WITHOUT following the redirect, and saves the Set-Cookie
(Next.js __prerender_bypass) to a 0600 JSON cookie file. Load that file into a
browser or curl to see the draft instead of publishing to look at it. The secret
comes from PAY_PREVIEW_SECRET[_<PROFILE>], the variable named by the profile's
preview_secret_env, or ` + "`pay auth preview-secret --stdin`" + `; it is never printed.`,
		Args: cobra.RangeArgs(1, 2),
	}
	cmd.RunE = runData(rt, safety.CmdURL, func(ctx context.Context, cmd *cobra.Command, d *Deps, args []string) (*output.Envelope, error) {
		id := ""
		if len(args) == 2 {
			id = args[1]
		}
		return runURL(ctx, d, f, args[0], id)
	})
	fl := cmd.Flags()
	fl.StringVar(&f.slug, "slug", "", "address the document by its slug instead of its id")
	fl.BoolVar(&f.draft, "draft", false, "resolve the path from the newest draft (a slug or parent changed in a draft)")
	fl.BoolVar(&f.preview, "preview", false, "perform the draft-preview handshake and save its cookies (implies --draft)")
	fl.StringVar(&f.cookieFile, "cookie-file", "", "where --preview writes the cookies (default: <state_dir>/preview/<profile>.cookies.json)")
	fl.StringVar(&f.locale, "locale", "", "read a localized slug/breadcrumbs in this locale; also fills {locale}")

	SetHelp(cmd, urlHelp())
	return cmd
}

func urlHelp() *Help {
	return &Help{
		Synopsis: []string{
			"pay url <collection> <id> [--draft] [--locale CODE]",
			"pay url <collection> --slug SLUG [--draft]",
			"pay url <collection> <id> --preview [--cookie-file FILE]",
		},
		Collections: true,
		Args: []ArgSpec{
			{Name: "collection", Required: true, Type: "enum", ValuesFrom: "discovery.collections", Example: "pages"},
			{Name: "id", Required: false, Type: "string", Example: "47"},
		},
		FlagInfo: map[string]FlagInfo{
			"slug":        {Note: "replaces <id>; 0 matches = doc_not_found (4), >1 = slug_ambiguous (5)"},
			"preview":     {Note: "needs a preview secret when preview_path contains {secret}; preview_secret_missing (9) otherwise"},
			"cookie-file": {Note: "written 0600; a JSON array of {name,value,domain,path,expires,httpOnly,secure,sameSite}"},
		},
		Output: OutputSpec{
			Kind: output.KindOpResult,
			Skeleton: `{"url":"https://site/skills/data/dashboards","path":"/skills/data/dashboards","path_source":"breadcrumbs",` +
				`"status":"published","has_unpublished_changes":false,"preview":{"cookie_file":"…","cookies":[{"name":"__prerender_bypass","domain":"site"}]}}`,
		},
		ExitCodes: []int{
			apierr.ExitOK, apierr.ExitInternal, apierr.ExitAuth, apierr.ExitThrottled,
			apierr.ExitNotFound, apierr.ExitValidation, apierr.ExitNetwork,
			apierr.ExitAccessDenied, apierr.ExitConfig, apierr.ExitCapability,
		},
		Examples: []Example{
			{Why: "the live URL of a page", Cmd: "pay url pages 47"},
			{Why: "the same, addressed by slug", Cmd: "pay url pages --slug dashboards"},
			{Why: "just the URL, for a script", Cmd: "pay url pages 47 --path .url --output id"},
			{Why: "where a draft WILL live (its slug or parent changed in the draft)", Cmd: "pay url pages 47 --draft"},
			{Why: "view a draft without publishing: mint the preview cookie", Cmd: "pay url pages 47 --preview"},
			{Why: "fetch the draft-mode page with the saved cookies",
				Cmd: `curl -sS -H "Cookie: $(jq -r 'map(.name+"="+.value)|join("; ")' ~/.local/state/pay/preview/default.cookies.json)" "$(pay url pages 47 --path .url --output id)"`},
			{Why: "teach posts their route once", Cmd: "pay config set profiles.default.url_paths.posts '/posts/{slug}'"},
		},
		Mistakes: []Mistake{
			{Wrong: "Publishing a page to stage just to screenshot it.",
				Right: "pay url <collection> <id> --preview mints Next.js' draft-mode cookie; load the cookie file into the browser and open data.preview.url."},
			{Wrong: "Trusting path_source \"slug\" for a collection that is not served at /{slug}.",
				Right: "It is a guess and says so (url_path_guessed). Pin the route: pay config set profiles.<p>.url_paths.<collection> '/posts/{slug}'."},
			{Wrong: "Putting the preview secret in pay.toml.",
				Right: "preview_secret is rejected in every config file (config_secret_in_plaintext). Export PAY_PREVIEW_SECRET, set preview_secret_env to the variable your site reads, or run pay auth preview-secret --stdin."},
			{Wrong: "Reading the cookie values from the envelope.",
				Right: "They are never printed; only names and domains are. The values are in the 0600 cookie file."},
		},
		SeeAlso: []string{
			"pay auth preview-secret   # where the preview secret comes from",
			"pay get <collection> --slug <slug>   # the document itself",
			"pay doctor   # site_url, url_paths and preview configuration",
		},
	}
}

// urlResult is `pay url`'s op_result.
type urlResult struct {
	URL           string  `json:"url"`
	Path          string  `json:"path"`
	PathSource    string  `json:"path_source"`
	PublishedPath *string `json:"published_path,omitempty"`
	SiteURL       string  `json:"site_url"`
	SiteURLSource string  `json:"site_url_source"`
	Collection    string  `json:"collection"`
	ID            any     `json:"id"`
	Slug          *string `json:"slug"`
	// Status is the stored _status: "published" when a published version
	// exists, "draft" when none does; null on a collection without drafts.
	Status *string `json:"status"`
	// HasUnpublishedChanges is true when the newest version is a draft.
	HasUnpublishedChanges *bool `json:"has_unpublished_changes"`
	// Draft says the path was resolved from the newest draft.
	Draft   bool           `json:"draft"`
	Preview *previewResult `json:"preview,omitempty"`
}

func runURL(ctx context.Context, d *Deps, f *urlFlags, collection, id string) (*output.Envelope, error) {
	cfg := d.cfg()
	switch {
	case id == "" && f.slug == "":
		return nil, apierr.New(apierr.CodeInvalidArgs, "`pay url` needs a document: pass <id> or --slug SLUG").
			WithHint("pay url <collection> <id>, or pay url <collection> --slug <slug>")
	case id != "" && f.slug != "":
		return nil, apierr.New(apierr.CodeInvalidArgs, "pass <id> or --slug, not both").
			WithHint("drop one of them: the id and the slug each address the document on their own")
	}
	if cfg.SiteURL == "" {
		return nil, apierr.New(apierr.CodeConfigMissing, "no site_url and no base_url are configured for profile %q", cfg.Profile).
			WithHint("pay config set profiles.%s.site_url https://www.example.com", cfg.Profile)
	}
	useDraft := f.draft || f.preview

	// Resolve the preview secret BEFORE any request, so a missing one fails in
	// zero round trips and a resolved one is scrubbed from everything after.
	var prev *secret.PreviewSecret
	if f.preview && strings.Contains(cfg.PreviewPath, "{secret}") {
		var err error
		prev, err = d.RT.PreviewSecret()
		if err != nil {
			return nil, err
		}
		if prev == nil {
			return nil, previewSecretMissing(cfg)
		}
	}

	client, err := d.requireClient()
	if err != nil {
		return nil, err
	}
	t, err := d.collection(ctx, collection)
	if err != nil {
		return nil, err
	}
	locale := f.locale
	if locale == "" {
		locale = cfg.Locale
	}
	if f.slug != "" {
		ref, err := d.ResolveDocBySlug(ctx, t, f.slug, SlugLookup{Draft: useDraft && hasDrafts(t), Locale: locale})
		if err != nil {
			return nil, err
		}
		id = ref.IDString
	} else if e := d.checkID(t, t.Slug, id); e != nil {
		return nil, e
	}

	run := d.beginRun()
	docs, err := fetchURLDocs(ctx, client, t, cfg, id, locale)
	if err != nil {
		return nil, err
	}
	source := docs.stored
	if useDraft && docs.newest != nil {
		source = docs.newest
	}
	tmpl := cfg.URLPaths[t.Slug]
	path, pathSource, err := resolveFrontendPath(t.Slug, source, tmpl, locale)
	if err != nil {
		return nil, withURLPathHint(err, cfg.Profile, t.Slug)
	}

	res := &urlResult{
		URL:                   joinSiteURL(cfg.SiteURL, path),
		Path:                  path,
		PathSource:            pathSource,
		SiteURL:               redact.URL(cfg.SiteURL),
		SiteURLSource:         cfg.Sources["site_url"],
		Collection:            t.Slug,
		ID:                    source.ID(),
		Status:                docs.status,
		HasUnpublishedChanges: docs.changed,
		Draft:                 useDraft && docs.newest != nil,
	}
	if s, ok := source["slug"].(string); ok && s != "" {
		res.Slug = &s
	}
	if res.Draft && docs.stored != nil && docs.stored["id"] != nil {
		if pubPath, _, err := resolveFrontendPath(t.Slug, docs.stored, tmpl, locale); err == nil && pubPath != path {
			res.PublishedPath = &pubPath
		}
	}

	var warnings []output.Warning
	if pathSource == PathSourceSlug && t.Slug != "pages" {
		warnings = append(warnings, output.Warning{
			Code: WarnURLPathGuessed,
			Message: fmt.Sprintf("%s has no url_paths entry and no breadcrumbs, so the path /{slug} is a guess; "+
				"the website template serves pages at /{slug} and posts at /posts/{slug}", t.Slug),
			Hint: fmt.Sprintf("pay config set profiles.%s.url_paths.%s '/%s/{slug}'", cfg.Profile, t.Slug, t.Slug),
		})
	}
	if docs.status != nil && *docs.status != "published" && !useDraft {
		warnings = append(warnings, output.Warning{
			Code: WarnURLNotLive,
			Message: fmt.Sprintf("%s %s has no published version, so this URL does not serve it to visitors yet",
				t.Slug, id),
			Hint: fmt.Sprintf("view the draft without publishing it: pay url %s %s --preview", t.Slug, id),
		})
	}

	if f.preview {
		p, warn, err := runPreviewHandshake(ctx, d, client, t.Slug, id, res, prev, f.cookieFile)
		if err != nil {
			return nil, err
		}
		res.Preview = p
		if warn != nil {
			warnings = append(warnings, *warn)
		}
	}

	env := output.New(safety.CmdURL, output.KindOpResult, jsonValue(res)).
		WithTarget(t.envTarget(source.ID())).
		WithMeta(run.meta())
	for _, w := range warnings {
		env.AddWarning(w)
	}
	return env, nil
}

func hasDrafts(t *collTarget) bool {
	d := t.flags().Drafts
	return d == nil || *d
}

// urlDocs is what fetchURLDocs learned about one document.
type urlDocs struct {
	// stored is the document a bare read returns: the published state, or
	// the never-published draft.
	stored payload.Doc
	// newest is the newest version (?draft=true); nil on a collection
	// without drafts.
	newest  payload.Doc
	status  *string
	changed *bool
}

// fetchURLDocs reads the newest version first. When that version is
// published it IS the stored document (Payload writes the main row on every
// published save), so one request answers everything; otherwise a bare read
// supplies the stored state. On a collection without drafts there is one
// read and every save is live.
func fetchURLDocs(ctx context.Context, client *payload.Client, t *collTarget, cfg *config.Resolved,
	id, locale string) (*urlDocs, error) {
	opts := []payload.Option{payload.WithClassify(classifyFor(t, cfg, nil)), payload.WithKnownRoute()}
	base := query.Params{Depth: query.IntPtr(0), Locale: locale}
	out := &urlDocs{}
	if !hasDrafts(t) {
		doc, _, err := client.Get(ctx, t.Slug, id, base, opts...)
		if err != nil {
			return nil, err
		}
		out.stored = doc
		no := false
		out.changed = &no
		return out, nil
	}
	draftP := base
	draftP.Draft = query.BoolPtr(true)
	newest, _, err := client.Get(ctx, t.Slug, id, draftP, opts...)
	if err != nil {
		return nil, err
	}
	out.newest = newest
	newestStatus, ok := newest["_status"].(string)
	if !ok {
		// The collection turned out to have no drafts after all (an unknown
		// flag): there is no status to report.
		out.stored, out.newest = newest, nil
		return out, nil
	}
	if newestStatus == "published" {
		out.stored = newest
		pub, no := "published", false
		out.status, out.changed = &pub, &no
		return out, nil
	}
	stored, _, err := client.Get(ctx, t.Slug, id, base, opts...)
	if err != nil {
		return nil, err
	}
	out.stored = stored
	status, _ := stored["_status"].(string)
	if status == "" {
		status = "draft"
	}
	yes := true
	out.status, out.changed = &status, &yes
	return out, nil
}

// resolveFrontendPath is F9's path ladder: breadcrumbs, then the url_paths
// template, then /{slug}; a resulting /home is served at "/". It is pure.
func resolveFrontendPath(collection string, doc payload.Doc, tmpl, locale string) (string, string, error) {
	path, source := "", ""
	if bc, ok := doc["breadcrumbs"].([]any); ok && len(bc) > 0 {
		if last, ok := bc[len(bc)-1].(map[string]any); ok {
			if u, ok := last["url"].(string); ok && strings.HasPrefix(u, "/") {
				path, source = u, PathSourceBreadcrumbs
			}
		}
	}
	if path == "" && tmpl != "" {
		rendered, err := expandTemplate(tmpl, func(key string) (string, bool) {
			if key == "locale" {
				return locale, locale != ""
			}
			return docValue(doc, key)
		}, pathEscaper)
		if err != nil {
			return "", "", apierr.Wrap(err, apierr.CodeInvalidOption,
				"url_paths.%s = %q cannot be filled from %s %v: %s", collection, tmpl, collection, doc.ID(), err.Error())
		}
		path, source = rendered, PathSourceTemplate
	}
	if path == "" {
		if s, ok := doc["slug"].(string); ok && s != "" {
			path, source = "/"+url.PathEscape(s), PathSourceSlug
		}
	}
	if path == "" {
		return "", "", apierr.New(apierr.CodeFeatureUnavailable,
			"cannot derive a frontend path for %s %v: it has no breadcrumbs, no slug, and the profile has no url_paths.%s template",
			collection, doc.ID(), collection)
	}
	if path == "/home" {
		path, source = "/", PathSourceHome
	}
	return path, source, nil
}

func withURLPathHint(err error, profile, collection string) error {
	e := apierr.From(err)
	if e.Hint == "" || e.Hint == apierr.DefaultHint(e.Code) {
		e = e.WithHint("pin the route: pay config set profiles.%s.url_paths.%s '/%s/{slug}'", profile, collection, collection)
	}
	return e
}

// docValue reads a dotted path of scalars from a document.
func docValue(doc payload.Doc, key string) (string, bool) {
	var cur any = map[string]any(doc)
	for _, seg := range strings.Split(key, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[seg]
		if !ok {
			return "", false
		}
	}
	switch v := cur.(type) {
	case string:
		return v, v != ""
	case float64:
		return fmt.Sprintf("%v", v), true
	case json.Number:
		return v.String(), true
	case int, int64:
		return fmt.Sprintf("%d", v), true
	case bool:
		return strconv.FormatBool(v), true
	}
	return "", false
}

// escaper encodes one substituted value; inQuery says the placeholder sits
// after the template's first '?'.
type escaper func(key, value string, inQuery bool) string

func pathEscaper(_, value string, inQuery bool) string {
	if inQuery {
		return url.QueryEscape(value)
	}
	return url.PathEscape(value)
}

// expandTemplate substitutes every {key} in tmpl. A key with no value is an
// error naming it: a URL with a hole in it is worse than no URL.
func expandTemplate(tmpl string, value func(key string) (string, bool), esc escaper) (string, error) {
	var b strings.Builder
	query := strings.Index(tmpl, "?")
	for i := 0; i < len(tmpl); {
		open := strings.IndexByte(tmpl[i:], '{')
		if open < 0 {
			b.WriteString(tmpl[i:])
			break
		}
		open += i
		end := strings.IndexByte(tmpl[open:], '}')
		if end < 0 {
			return "", errors.New("unclosed {")
		}
		end += open
		b.WriteString(tmpl[i:open])
		key := tmpl[open+1 : end]
		v, ok := value(key)
		if !ok {
			return "", fmt.Errorf("{%s} has no value", key)
		}
		b.WriteString(esc(key, v, query >= 0 && open > query))
		i = end + 1
	}
	return b.String(), nil
}

// joinSiteURL appends an absolute path to site_url, which may carry a base
// path of its own (a Next.js basePath).
func joinSiteURL(site, path string) string {
	return strings.TrimSuffix(site, "/") + path
}
