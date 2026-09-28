package config

import (
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
	"github.com/KLIXPERT-io/pay-cli/internal/redact"
)

func siteInput(project, user Profile) Input {
	return Input{
		Flags:   Flags{Profile: "stage"},
		Env:     Env{},
		Project: &File{Path: "/repo/pay.toml", Kind: KindProject, Exists: true, Profiles: map[string]Profile{"stage": project}},
		User:    &File{Path: "/home/u/.config/pay/config.toml", Kind: KindUser, Exists: true, Profiles: map[string]Profile{"stage": user}},
	}
}

// F9: with nothing configured the site is base_url's ORIGIN (a base_url with a
// path prefix does not move the site), and the preview route is the website
// template's.
func TestSiteDefaultsDeriveFromBaseURL(t *testing.T) {
	r, err := Resolve(siteInput(Profile{}, Profile{BaseURL: "https://cms.example.com/admin-api"}))
	if err != nil {
		t.Fatal(err)
	}
	if r.SiteURL != "https://cms.example.com" || r.Sources["site_url"] != SourceDerivedSiteURL {
		t.Fatalf("site_url = %q from %q", r.SiteURL, r.Sources["site_url"])
	}
	if r.PreviewPath != DefaultPreviewPath || r.Sources["preview_path"] != SourceDefault {
		t.Fatalf("preview_path = %q from %q", r.PreviewPath, r.Sources["preview_path"])
	}
	if r.URLPaths != nil || r.PreviewSecretEnv != "" {
		t.Fatalf("unexpected defaults: %+v %q", r.URLPaths, r.PreviewSecretEnv)
	}
}

func TestSiteSettingsFollowThePrecedenceChain(t *testing.T) {
	in := siteInput(
		Profile{SiteURL: "https://www.example.com/", URLPaths: map[string]string{"posts": "/blog/{slug}"}, PreviewSecretEnv: "PREVIEW_SECRET"},
		Profile{BaseURL: "https://cms.example.com", SiteURL: "https://user.example.com",
			URLPaths: map[string]string{"posts": "/posts/{slug}", "jobs": "/karriere/{slug}"}, PreviewPath: "/api/preview?slug={path}&token={secret}"},
	)
	r, err := Resolve(in)
	if err != nil {
		t.Fatal(err)
	}
	if r.SiteURL != "https://www.example.com" || !strings.HasPrefix(r.Sources["site_url"], "project:") {
		t.Fatalf("site_url = %q from %q", r.SiteURL, r.Sources["site_url"])
	}
	if r.URLPaths["posts"] != "/blog/{slug}" || r.URLPaths["jobs"] != "/karriere/{slug}" {
		t.Fatalf("url_paths must merge per key with the project winning: %v", r.URLPaths)
	}
	if r.PreviewPath != "/api/preview?slug={path}&token={secret}" || r.PreviewSecretEnv != "PREVIEW_SECRET" {
		t.Fatalf("preview = %q / %q", r.PreviewPath, r.PreviewSecretEnv)
	}

	in.Env = Env{"PAY_SITE_URL": "http://localhost:3000"}
	r, err = Resolve(in)
	if err != nil || r.SiteURL != "http://localhost:3000" || r.Sources["site_url"] != "env:PAY_SITE_URL" {
		t.Fatalf("PAY_SITE_URL: %q from %q (%v)", r.SiteURL, r.Sources["site_url"], err)
	}
	ex := r.Explain()
	for _, key := range []string{"site_url", "url_paths", "preview_path", "preview_secret_env"} {
		if _, ok := ex[key]; !ok {
			t.Errorf("config explain is missing %s", key)
		}
	}
}

func TestSiteSettingsAreValidated(t *testing.T) {
	for name, p := range map[string]Profile{
		"relative site_url":        {SiteURL: "www.example.com"},
		"site_url with userinfo":   {SiteURL: "https://u:p@www.example.com"},
		"site_url with a query":    {SiteURL: "https://www.example.com/?a=1"},
		"relative preview_path":    {PreviewPath: "next/preview?path={path}"},
		"relative url_paths entry": {URLPaths: map[string]string{"posts": "posts/{slug}"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Resolve(siteInput(p, Profile{BaseURL: "https://cms.example.com"}))
			if apierr.CodeOf(err) != apierr.CodeInvalidOption {
				t.Fatalf("err = %v, want invalid_option", err)
			}
			if strings.Contains(err.Error(), "u:p@") {
				t.Fatalf("the error echoes credentials: %v", err)
			}
		})
	}
}

// preview_secret is a credential: like api_key it is a hard error in any
// config file, at any depth, because ./pay.toml is committed.
func TestPreviewSecretIsForbiddenInConfig(t *testing.T) {
	for _, doc := range []string{
		"[profiles.stage]\npreview_secret = \"x\"\n",
		"preview_secret = \"x\"\n",
		"[profiles.stage.nested]\npreview_secret = \"x\"\n",
	} {
		_, err := Parse([]byte(doc), "/repo/pay.toml", KindProject)
		if apierr.CodeOf(err) != apierr.CodeConfigSecretInPlaintext {
			t.Fatalf("%q: err = %v, want config_secret_in_plaintext", doc, err)
		}
		e, _ := apierr.As(err)
		if !strings.Contains(e.Hint, "pay auth preview-secret --stdin") {
			t.Errorf("hint does not name the storing command: %q", e.Hint)
		}
	}
	// The NAME of the variable is fine.
	if _, err := Parse([]byte("[profiles.stage]\npreview_secret_env = \"PREVIEW_SECRET\"\n"), "/repo/pay.toml", KindProject); err != nil {
		t.Fatalf("preview_secret_env must parse: %v", err)
	}
}

// The forbidden-key messages survive the §5.3 text scrubber every error
// message passes through: "sets KEY: a …" read as an assignment and lost the
// word after the colon ("preview_secret: <redacted> preview secret may never").
func TestForbiddenKeyMessagesSurviveRedaction(t *testing.T) {
	for _, doc := range []string{
		"[profiles.default]\npreview_secret = \"x\"\n",
		"[profiles.default]\napi_key = \"x\"\n",
		"api_key = \"x\"\n",
	} {
		_, err := Parse([]byte(doc), "/repo/pay.toml", KindProject)
		e, ok := apierr.As(err)
		if !ok {
			t.Fatalf("%q: err = %v", doc, err)
		}
		if got := redact.Text(e.Message); got != e.Message || strings.Contains(got, redact.Mask) {
			t.Errorf("%q: the scrubber rewrote the message:\n got %s\nwant %s", doc, got, e.Message)
		}
	}
}
