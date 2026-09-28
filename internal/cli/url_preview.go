package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/config"
	"github.com/KLIXPERT-io/pay-cli/internal/fsatomic"
	"github.com/KLIXPERT-io/pay-cli/internal/output"
	"github.com/KLIXPERT-io/pay-cli/internal/payload"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
	"github.com/KLIXPERT-io/pay-cli/internal/secret"
)

// ---------------------------------------------------------------------------
// the preview handshake (pay url --preview)
// ---------------------------------------------------------------------------

type previewResult struct {
	// URL is the page to open with the cookies loaded.
	URL        string       `json:"url"`
	CookieFile string       `json:"cookie_file"`
	Cookies    []cookieInfo `json:"cookies"`
	Handshake  handshake    `json:"handshake"`
	// KeySource names where the preview secret came from, KeyFingerprint is
	// its §4.4 fingerprint; never the value. (A key containing "secret" would
	// be masked by §5.3's structural rule, which is why these are key_*.)
	KeySource      string            `json:"key_source,omitempty"`
	KeyFingerprint string            `json:"key_fingerprint,omitempty"`
	Load           map[string]string `json:"load"`
}

// cookieInfo is a cookie WITHOUT its value.
type cookieInfo struct {
	Name     string  `json:"name"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  *string `json:"expires"`
	HTTPOnly bool    `json:"http_only"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"same_site"`
}

type handshake struct {
	// URL is the handshake URL with the secret replaced by <redacted>.
	URL            string `json:"url"`
	Status         int    `json:"status"`
	Location       string `json:"location,omitempty"`
	CredentialSent bool   `json:"credential_sent"`
}

// cookieRecord is one entry of the cookie file: the Playwright / agent-browser
// cookie shape, so the file loads as-is.
type cookieRecord struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Expires  int64  `json:"expires"` // unix seconds; -1 = session cookie
	HTTPOnly bool   `json:"httpOnly"`
	Secure   bool   `json:"secure"`
	SameSite string `json:"sameSite"`
}

func previewSecretMissing(cfg *config.Resolved) error {
	names := secret.PreviewSecretEnvNames(cfg.Profile)
	where := "$" + strings.Join(names, ", $")
	if cfg.PreviewSecretEnv != "" {
		where += ", $" + cfg.PreviewSecretEnv + " (preview_secret_env, currently unset)"
	} else {
		where += ", the variable named by preview_secret_env (not set)"
	}
	// The wording avoids "name=value" shapes around the word secret on
	// purpose: §5.3's text scrubber masks those, which would garble the very
	// instructions this error exists to give.
	return apierr.New(apierr.CodePreviewSecretMissing,
		"no preview secret is configured for profile %q, and its preview_path uses {secret}. Looked in: %s, and credentials.json",
		cfg.Profile, where).
		WithHint("export PAY_PREVIEW_SECRET with the value of the site's own PREVIEW_SECRET; or `pay config set profiles.%s.preview_secret_env PREVIEW_SECRET` "+
			"to reuse the variable the site reads; or store it once: printf %%s \"$PREVIEW_SECRET\" | pay auth preview-secret --stdin --profile %s",
			cfg.Profile, cfg.Profile)
}

// runPreviewHandshake GETs preview_path on the site without following the
// redirect, keeps the Set-Cookie headers, and writes them to the cookie file.
func runPreviewHandshake(ctx context.Context, d *Deps, client *payload.Client, collection, id string,
	res *urlResult, prev *secret.PreviewSecret, cookieFile string) (*previewResult, *output.Warning, error) {
	cfg := d.cfg()
	values := func(secretValue string) func(string) (string, bool) {
		return func(key string) (string, bool) {
			switch key {
			case "path":
				return res.Path, true
			case "secret":
				return secretValue, secretValue != ""
			case "collection":
				return collection, true
			case "id":
				return id, true
			case "slug":
				if res.Slug != nil {
					return *res.Slug, true
				}
				return "", false
			}
			return "", false
		}
	}
	esc := func(key, value string, inQuery bool) string {
		if value == redact.Mask {
			return value
		}
		if key == "path" && !inQuery {
			return value // already a URL path
		}
		return pathEscaper(key, value, inQuery)
	}
	secretValue := ""
	var secrets []string
	if prev != nil {
		secretValue = prev.Value
		secrets = []string{prev.Value}
	}
	rel, err := expandTemplate(cfg.PreviewPath, values(secretValue), esc)
	if err != nil {
		return nil, nil, apierr.New(apierr.CodeInvalidOption, "the profile's preview_path cannot be filled: %s", err.Error()).
			WithHint("placeholders are {path}, {secret}, {collection}, {id} and {slug}; "+
				"`pay config unset profiles.%s.preview_path` restores the website template's default", cfg.Profile)
	}
	shownRel, _ := expandTemplate(cfg.PreviewPath, values(redact.Mask), esc)
	handshakeURL := strings.TrimSuffix(cfg.SiteURL, "/") + rel
	shown := redact.URLWithSecrets(strings.TrimSuffix(cfg.SiteURL, "/")+shownRel, secrets)

	got, err := client.SiteGet(ctx, payload.SiteRequest{URL: handshakeURL, Secrets: secrets})
	if err != nil {
		return nil, nil, err
	}
	info := handshake{URL: shown, Status: got.Status, Location: got.Location, CredentialSent: !got.CrossOrigin && !d.RT.anonymous()}
	now := d.now()
	records, infos := cookieRecords(got.Cookies, handshakeURL, now)

	switch {
	case got.Status == http.StatusForbidden || got.Status == http.StatusUnauthorized:
		why := "the preview secret does not match the site's, or the preview route did not accept this profile's credential as a logged-in user"
		if got.CrossOrigin {
			why = "site_url's origin differs from base_url's, so PayCLI withheld the credential and the route saw no logged-in user (or the secret does not match)"
		}
		e := apierr.New(apierr.CodePreviewDenied, "the site refused the preview handshake (HTTP %d at %s): %s", got.Status, shown, why)
		if got.BodyExcerpt != "" {
			e = apierr.New(apierr.CodePreviewDenied, "the site refused the preview handshake (HTTP %d at %s: %q): %s",
				got.Status, shown, got.BodyExcerpt, why)
		}
		return nil, nil, e.WithHint("compare the secret's fingerprint and source with `pay auth preview-secret --profile %s`, and check the credential with `pay auth test --profile %s`",
			cfg.Profile, cfg.Profile)
	case got.Status == http.StatusNotFound:
		return nil, nil, apierr.New(apierr.CodePreviewFailed, "the site has no preview route at %s (HTTP 404)", shown).
			WithHint("point preview_path at the site's route, with {path} and {secret} placeholders: pay config set profiles.%s.preview_path '<route>'; "+
				"`pay config unset profiles.%s.preview_path` restores the website template's default", cfg.Profile, cfg.Profile)
	case len(records) == 0:
		detail := ""
		if got.BodyExcerpt != "" {
			detail = fmt.Sprintf(" (%q)", got.BodyExcerpt)
		}
		return nil, nil, apierr.New(apierr.CodePreviewFailed,
			"the site answered the preview handshake at %s with HTTP %d and set no cookie%s", shown, got.Status, detail).
			WithHint("check preview_path and site_url with `pay config explain`; the route must enable Next.js draft mode and redirect")
	}

	path, err := previewCookiePath(d, cookieFile)
	if err != nil {
		return nil, nil, err
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return nil, nil, apierr.Wrap(err, apierr.CodeInternal, "cannot encode the preview cookies")
	}
	if err := fsatomic.Write(path, append(data, '\n'), CookieFilePerm); err != nil {
		return nil, nil, apierr.Wrap(err, apierr.CodeInternal, "cannot write the preview cookie file %s", path)
	}

	out := &previewResult{
		URL:        res.URL,
		CookieFile: path,
		Cookies:    infos,
		Handshake:  info,
		Load: map[string]string{
			// --domain is required: agent-browser's JSON import rejects the
			// file with "Invalid cookie fields" without it (verified with
			// agent-browser against the website template, 2026-09-28).
			"agent_browser": fmt.Sprintf("agent-browser cookies set --curl %s --domain %s && agent-browser open %s",
				shellQuote(path), shellQuote(siteHost(cfg.SiteURL)), shellQuote(res.URL)),
			"curl": fmt.Sprintf(`curl -sS -H "Cookie: $(jq -r 'map(.name+"="+.value)|join("; ")' %s)" %s`,
				shellQuote(path), shellQuote(res.URL)),
		},
	}
	if prev != nil {
		out.KeySource, out.KeyFingerprint = prev.Source, prev.Fingerprint
	}
	var warn *output.Warning
	if got.CrossOrigin {
		warn = &output.Warning{
			Code: WarnPreviewNoAuth,
			Message: "site_url's origin differs from base_url's, so the handshake was sent without the profile's credential; " +
				"it worked because the preview route does not require a logged-in user",
		}
	}
	return out, warn, nil
}

// siteHost is the host name cookies are scoped to.
func siteHost(site string) string {
	if u, err := url.Parse(site); err == nil {
		return u.Hostname()
	}
	return site
}

// anonymous reports whether this invocation resolved no credential.
func (rt *Runtime) anonymous() bool {
	return rt == nil || rt.cred == nil || rt.cred.Anonymous()
}

// cookieRecords converts Set-Cookie headers into the cookie file's records and
// the envelope's value-free descriptions. A cookie the response DELETES
// (Max-Age<0, or an expiry in the past) is dropped: it grants nothing.
func cookieRecords(cookies []*http.Cookie, requestURL string, now time.Time) ([]cookieRecord, []cookieInfo) {
	host, defaultPath := "", "/"
	if u, err := url.Parse(requestURL); err == nil {
		host = u.Hostname()
		// RFC 6265 §5.1.4 default-path: the request path up to its last "/".
		if i := strings.LastIndex(u.Path, "/"); i > 0 {
			defaultPath = u.Path[:i]
		}
	}
	records := []cookieRecord{}
	infos := []cookieInfo{}
	for _, c := range cookies {
		if c == nil || c.Name == "" || c.MaxAge < 0 {
			continue
		}
		expires := int64(-1)
		switch {
		case c.MaxAge > 0:
			expires = now.Add(time.Duration(c.MaxAge) * time.Second).Unix()
		case !c.Expires.IsZero():
			if !now.IsZero() && c.Expires.Before(now) {
				continue
			}
			expires = c.Expires.Unix()
		}
		// A Set-Cookie without Domain is a host-only cookie for the host that
		// set it; a browser import needs that host spelled out.
		domain := c.Domain
		if domain == "" {
			domain = host
		}
		p := c.Path
		if p == "" {
			p = defaultPath
		}
		sameSite := "Lax"
		switch c.SameSite {
		case http.SameSiteStrictMode:
			sameSite = "Strict"
		case http.SameSiteNoneMode:
			sameSite = "None"
		}
		records = append(records, cookieRecord{Name: c.Name, Value: c.Value, Domain: domain, Path: p,
			Expires: expires, HTTPOnly: c.HttpOnly, Secure: c.Secure, SameSite: sameSite})
		info := cookieInfo{Name: c.Name, Domain: domain, Path: p, HTTPOnly: c.HttpOnly, Secure: c.Secure, SameSite: sameSite}
		if expires > 0 {
			s := time.Unix(expires, 0).UTC().Format(time.RFC3339)
			info.Expires = &s
		}
		infos = append(infos, info)
	}
	return records, infos
}

// previewCookiePath is --cookie-file (relative paths resolve against the
// working directory) or <state_dir>/preview/<profile>.cookies.json.
func previewCookiePath(d *Deps, flag string) (string, error) {
	if flag != "" {
		if filepath.IsAbs(flag) || d.RT == nil || d.RT.App.WorkDir == "" {
			return flag, nil
		}
		return filepath.Join(d.RT.App.WorkDir, flag), nil
	}
	if d.RT == nil || d.RT.Paths.StateDir == "" {
		return "", apierr.New(apierr.CodeConfigMissing, "no state directory is known, so the cookie file has nowhere to go").
			WithHint("pass --cookie-file FILE")
	}
	return filepath.Join(d.RT.Paths.StateDir, "preview", safeFileName(d.cfg().Profile)+".cookies.json"), nil
}

// safeFileName keeps a profile name usable as a file name on every OS.
func safeFileName(name string) string {
	if name == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
