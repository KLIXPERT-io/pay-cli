package redact

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// The fixture credential from docs/GROUNDING.md. §3.1's repo-wide secret
// assertion keys off exactly this literal, so every test here uses it.
const fixtureKey = "paycli-dev-key-deadbeefdeadbeef"

const fixtureJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpZCI6MSwiY29sbGVjdGlvbiI6InVzZXJzIn0.ZmFrZXNpZ25hdHVyZQ"

func TestKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"apiKey", "apiKey", true},
		{"apiKey lowercase", "apikey", true},
		{"apiKey uppercase", "APIKEY", true},
		{"apiKeyIndex", "apiKeyIndex", true},
		{"hash", "hash", true},
		{"salt", "salt", true},
		{"password", "password", true},
		{"sessions", "sessions", true},
		{"resetPasswordToken", "resetPasswordToken", true},
		{"resetPasswordExpiration", "resetPasswordExpiration", true},
		{"token substring", "token", true},
		{"refreshToken substring", "refreshToken", true},
		// §5.3 requires `pay auth status` to PRINT token_exp; an expiry
		// timestamp is not a credential. See TestTokenExpIsNotASecret.
		{"token_exp is an expiry, not a token", "token_exp", false},
		{"secret substring", "payloadSecret", true},
		{"client_secret", "client_secret", true},
		{"plain field", "title", false},
		{"slug", "slug", false},
		{"id", "id", false},
		{"createdAt", "createdAt", false},
		{"email is not a secret", "email", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Key(tc.key); got != tc.want {
				t.Errorf("Key(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

func TestHeader(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"Authorization", true},
		{"authorization", true},
		{"AUTHORIZATION", true},
		{"Proxy-Authorization", true},
		{"Cookie", true},
		{"Set-Cookie", true},
		{"X-Api-Key", true},
		{"x-api-key", true},
		{"api-key", true},
		{"api_key", true},
		{"apikey", true},
		{"X-Payload-Migration-Token", true},
		{"Content-Type", false},
		{"Accept-Language", false},
		{"User-Agent", false},
		{"X-Request-Id", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Header(tc.name); got != tc.want {
				t.Errorf("Header(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestJSONByteFaithful is the §11.1 rule: when nothing needed redacting the
// caller gets the exact input bytes back — no reindenting, no key reordering,
// no number normalisation.
func TestJSONByteFaithful(t *testing.T) {
	inputs := []string{
		`{"id":11,"title":"Home","slug":"home","_status":"published"}`,
		"{\n  \"id\": 11,\n  \"nested\": { \"z\": 1, \"a\": [1, 2, 3] }\n}",
		`{"big":10000000000000000000000000,"exp":1.0e2,"neg":-0.0}`,
		`[]`,
		`{}`,
		`"a bare string"`,
		`null`,
		`{"unicode":"héllo é \n tab\t","html":"<b>&</b>"}`,
		`{"dupe":1,"dupe":2}`,
	}
	for _, in := range inputs {
		t.Run(in[:min(len(in), 28)], func(t *testing.T) {
			got := JSON([]byte(in))
			if got.Changed {
				t.Fatalf("Changed = true, want false (paths %v)", got.Paths)
			}
			if string(got.Data) != in {
				t.Errorf("Data = %q, want the input bytes %q", got.Data, in)
			}
		})
	}
}

func TestJSONRedaction(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		want      string
		wantPaths []string
	}{
		{
			name:      "users/me leaks the api key",
			in:        `{"user":{"id":1,"email":"a@b.c","apiKey":"` + fixtureKey + `","apiKeyIndex":"deadbeef","enableAPIKey":true}}`,
			want:      `{"user":{"id":1,"email":"a@b.c","apiKey":"<redacted>","apiKeyIndex":"<redacted>","enableAPIKey":true}}`,
			wantPaths: []string{"user.apiKey", "user.apiKeyIndex"},
		},
		{
			name:      "bulk update echoes every user",
			in:        `{"docs":[{"id":1,"apiKey":"` + fixtureKey + `"},{"id":2,"apiKey":"other-secret-value"}],"errors":[]}`,
			want:      `{"docs":[{"id":1,"apiKey":"<redacted>"},{"id":2,"apiKey":"<redacted>"}],"errors":[]}`,
			wantPaths: []string{"docs[0].apiKey", "docs[1].apiKey"},
		},
		{
			name:      "whole sessions array is replaced",
			in:        `{"id":1,"sessions":[{"id":"s1","createdAt":"x"},{"id":"s2"}],"title":"t"}`,
			want:      `{"id":1,"sessions":"<redacted>","title":"t"}`,
			wantPaths: []string{"sessions"},
		},
		{
			name:      "hash and salt",
			in:        `{"hash":"abc","salt":"def","loginAttempts":0}`,
			want:      `{"hash":"<redacted>","salt":"<redacted>","loginAttempts":0}`,
			wantPaths: []string{"hash", "salt"},
		},
		{
			name:      "login response token",
			in:        `{"exp":1,"token":"` + fixtureJWT + `","user":{"id":1}}`,
			want:      `{"exp":1,"token":"<redacted>","user":{"id":1}}`,
			wantPaths: []string{"token"},
		},
		{
			name:      "bare JWT under an innocent key",
			in:        `{"note":"` + fixtureJWT + `"}`,
			want:      `{"note":"<redacted>"}`,
			wantPaths: []string{"note"},
		},
		{
			name:      "JWT inside an array",
			in:        `{"items":["ok","` + fixtureJWT + `"]}`,
			want:      `{"items":["ok","<redacted>"]}`,
			wantPaths: []string{"items[1]"},
		},
		{
			name:      "pretty printing outside the span is preserved",
			in:        "{\n  \"title\": \"Home\",\n  \"apiKey\": \"" + fixtureKey + "\",\n  \"n\": 1\n}",
			want:      "{\n  \"title\": \"Home\",\n  \"apiKey\": \"<redacted>\",\n  \"n\": 1\n}",
			wantPaths: []string{"apiKey"},
		},
		{
			name:      "non-string secret value",
			in:        `{"resetPasswordExpiration":1758000000000,"id":1}`,
			want:      `{"resetPasswordExpiration":"<redacted>","id":1}`,
			wantPaths: []string{"resetPasswordExpiration"},
		},
		{
			name:      "null secret is still masked",
			in:        `{"resetPasswordToken":null}`,
			want:      `{"resetPasswordToken":"<redacted>"}`,
			wantPaths: []string{"resetPasswordToken"},
		},
		{
			name:      "deeply nested",
			in:        `{"a":{"b":{"c":[{"d":{"apiKey":"` + fixtureKey + `"}}]}}}`,
			want:      `{"a":{"b":{"c":[{"d":{"apiKey":"<redacted>"}}]}}}`,
			wantPaths: []string{"a.b.c[0].d.apiKey"},
		},
		{
			name:      "root-level secret",
			in:        `{"apiKey":"` + fixtureKey + `"}`,
			want:      `{"apiKey":"<redacted>"}`,
			wantPaths: []string{"apiKey"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := JSON([]byte(tc.in))
			if !got.Changed {
				t.Fatal("Changed = false, want true")
			}
			if string(got.Data) != tc.want {
				t.Errorf("Data mismatch\n got: %s\nwant: %s", got.Data, tc.want)
			}
			if diff := cmp.Diff(tc.wantPaths, got.Paths); diff != "" {
				t.Errorf("Paths (-want +got):\n%s", diff)
			}
			// The output must still be valid JSON.
			var v any
			if err := json.Unmarshal(got.Data, &v); err != nil {
				t.Errorf("redacted output is not valid JSON: %v", err)
			}
			if strings.Contains(string(got.Data), fixtureKey) {
				t.Error("fixture key survived redaction")
			}
			if strings.Contains(string(got.Data), fixtureJWT) {
				t.Error("fixture JWT survived redaction")
			}
		})
	}
}

func TestJSONNonJSONFallback(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantChange bool
		wantAbsent string
	}{
		{
			name:       "next.js html error page with a token in it",
			in:         `<html id="__next_error__">Authorization: JWT ` + fixtureJWT + `</html>`,
			wantChange: true,
			wantAbsent: fixtureJWT,
		},
		{name: "plain html", in: `<html>nothing here</html>`},
		{name: "truncated json falls back to text scrubbing", in: "{\"apiKey\":\"" + fixtureKey, wantChange: true, wantAbsent: fixtureKey},
		{name: "empty", in: ``},
		{name: "whitespace", in: "   \n"},
		{name: "trailing garbage", in: `{}<html>`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := JSON([]byte(tc.in))
			if got.Changed != tc.wantChange {
				t.Errorf("Changed = %v, want %v (data %q)", got.Changed, tc.wantChange, got.Data)
			}
			if !tc.wantChange && string(got.Data) != tc.in {
				t.Errorf("unchanged body must be byte-faithful, got %q", got.Data)
			}
			if tc.wantAbsent != "" && strings.Contains(string(got.Data), tc.wantAbsent) {
				t.Errorf("secret survived: %q", got.Data)
			}
		})
	}
}

func TestScrubberLiterals(t *testing.T) {
	s := Scrubber{Literals: []string{fixtureKey, "short"}}
	t.Run("json under an innocent key", func(t *testing.T) {
		got := s.JSON([]byte(`{"note":"the key is ` + fixtureKey + ` ok"}`))
		if !got.Changed {
			t.Fatal("Changed = false")
		}
		if strings.Contains(string(got.Data), fixtureKey) {
			t.Errorf("literal survived: %s", got.Data)
		}
	})
	t.Run("text", func(t *testing.T) {
		got := s.Text("curl -H 'x: " + fixtureKey + "'")
		if strings.Contains(got, fixtureKey) {
			t.Errorf("literal survived: %s", got)
		}
	})
	t.Run("short literals are ignored", func(t *testing.T) {
		if got := s.Text("a short word"); got != "a short word" {
			t.Errorf("short literal was applied: %q", got)
		}
	})
}

func TestText(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantAbsent string
		want       string
	}{
		{
			name: "authorization header line",
			in:   "Authorization: users API-Key " + fixtureKey,
			want: "Authorization: <redacted>",
		},
		{
			name: "jwt in a sentence",
			in:   "token was " + fixtureJWT + " and it expired",
			want: "token was <redacted> and it expired",
		},
		{
			name: "url userinfo",
			in:   "GET http://admin:hunter2@localhost:3900/api/pages failed",
			want: "GET http://<redacted>@localhost:3900/api/pages failed",
		},
		{
			name: "api-key assignment",
			in:   "?api-key=" + fixtureKey + "&depth=0",
			want: "?api-key=<redacted>&depth=0",
		},
		{
			name: "password assignment",
			in:   `{"password": "hunter2"}`,
			want: `{"password": <redacted>}`,
		},
		{
			name: "nothing to redact",
			in:   "GET /api/pages?limit=3 -> 200",
			want: "GET /api/pages?limit=3 -> 200",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Text(tc.in)
			if tc.want != "" && got != tc.want {
				t.Errorf("Text() = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, fixtureKey) || strings.Contains(got, fixtureJWT) {
				t.Errorf("secret survived: %q", got)
			}
		})
	}
}

func TestURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no credentials", "http://localhost:3900/api/pages?limit=3", "http://localhost:3900/api/pages?limit=3"},
		{"user and password", "https://admin:hunter2@example.com/api", "https://example.com/api"},
		{"user only", "https://admin@example.com/api", "https://example.com/api"},
		{"api-key query param", "http://h/api?api-key=" + fixtureKey, "http://h/api?api-key=<redacted>"},
		{"token query param", "http://h/api?token=abc&depth=0", "http://h/api?token=<redacted>&depth=0"},
		{"apiKey query param camel", "http://h/api?apiKey=abc", "http://h/api?apiKey=<redacted>"},
		{"param order preserved", "http://h/a?z=1&secret=s&a=2", "http://h/a?z=1&secret=<redacted>&a=2"},
		{"valueless secret param", "http://h/a?token", "http://h/a?token"},
		{"encoded where clause is untouched", "http://h/api/pages?where%5Bslug%5D%5Bequals%5D=home", "http://h/api/pages?where%5Bslug%5D%5Bequals%5D=home"},
		{"jwt in the path", "http://h/reset/" + fixtureJWT, "http://h/reset/<redacted>"},
		{"both userinfo and param", "http://u:p@h/api?api_key=x", "http://h/api?api_key=<redacted>"},
		{"empty", "", ""},
		{"not a url", "::::not a url::::", "::::not a url::::"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := URL(tc.in); got != tc.want {
				t.Errorf("URL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestHeaders(t *testing.T) {
	h := http.Header{
		"Authorization": {"users API-Key " + fixtureKey},
		"Cookie":        {"payload-token=" + fixtureJWT},
		"Content-Type":  {"application/json"},
		"X-Tenant":      {"acme"},
		"X-Api-Key":     {fixtureKey},
	}
	got := Headers(h, "X-Tenant")

	if v := got.Get("Authorization"); !strings.HasPrefix(v, "<redacted:fp=") || !strings.HasSuffix(v, ">") {
		t.Errorf("Authorization = %q, want the fingerprint form", v)
	}
	if v := got.Get("Cookie"); v != Mask {
		t.Errorf("Cookie = %q", v)
	}
	if v := got.Get("X-Api-Key"); v != Mask {
		t.Errorf("X-Api-Key = %q", v)
	}
	if v := got.Get("X-Tenant"); v != Mask {
		t.Errorf("configured header X-Tenant = %q, want masked", v)
	}
	if v := got.Get("Content-Type"); v != "application/json" {
		t.Errorf("Content-Type = %q, want it untouched", v)
	}
	// The input must not be mutated.
	if h.Get("Authorization") != "users API-Key "+fixtureKey {
		t.Error("Headers mutated its input")
	}
	for name, values := range got {
		for _, v := range values {
			if strings.Contains(v, fixtureKey) || strings.Contains(v, fixtureJWT) {
				t.Errorf("secret survived in %s: %q", name, v)
			}
		}
	}
	if Headers(nil) != nil {
		t.Error("Headers(nil) must be nil")
	}
}

func TestAuthorizationValueAndFingerprint(t *testing.T) {
	apiKey := AuthorizationValue("users API-Key " + fixtureKey)
	jwt := AuthorizationValue("JWT " + fixtureKey)
	bearer := AuthorizationValue("Bearer " + fixtureKey)
	if apiKey != jwt || jwt != bearer {
		t.Errorf("the scheme must not change the fingerprint: %q %q %q", apiKey, jwt, bearer)
	}
	if want := "<redacted:fp=" + Fingerprint(fixtureKey) + ">"; apiKey != want {
		t.Errorf("AuthorizationValue = %q, want %q", apiKey, want)
	}
	if got := len(Fingerprint(fixtureKey)); got != 16 {
		t.Errorf("Fingerprint length = %d, want 16", got)
	}
	if Fingerprint("a") == Fingerprint("b") {
		t.Error("Fingerprint collides on trivial inputs")
	}
	if Fingerprint(fixtureKey) != Fingerprint(fixtureKey) {
		t.Error("Fingerprint is not stable")
	}
	if strings.Contains(Fingerprint(fixtureKey), fixtureKey) {
		t.Error("Fingerprint leaks the secret")
	}
	for _, in := range []string{"", "   ", "JWT", "JWT "} {
		if got := AuthorizationValue(in); got != Mask {
			t.Errorf("AuthorizationValue(%q) = %q, want %q", in, got, Mask)
		}
	}
	if got := MaskSecret(""); got != Mask {
		t.Errorf("MaskSecret(\"\") = %q", got)
	}
}

func TestValue(t *testing.T) {
	in := map[string]any{
		"id":    float64(1),
		"title": "Home",
		"auth":  map[string]any{"apiKey": fixtureKey, "hash": "h"},
		"list":  []any{map[string]any{"salt": "s"}, "plain", fixtureJWT},
	}
	got, paths := Value(in)
	want := map[string]any{
		"id":    float64(1),
		"title": "Home",
		"auth":  map[string]any{"apiKey": Mask, "hash": Mask},
		"list":  []any{map[string]any{"salt": Mask}, "plain", Mask},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Value (-want +got):\n%s", diff)
	}
	wantPaths := []string{"auth.apiKey", "auth.hash", "list[0].salt", "list[2]"}
	if diff := cmp.Diff(wantPaths, paths); diff != "" {
		t.Errorf("paths (-want +got):\n%s", diff)
	}
	// The input map must not be mutated.
	if in["auth"].(map[string]any)["apiKey"] != fixtureKey {
		t.Error("Value mutated its input")
	}
}

func TestIsJWT(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{fixtureJWT, true},
		{"eyJhbGciOiJIUzI1NiJ9.eyJhIjoxfQ.sig", true},
		{"eyJhbGciOiJIUzI1NiJ9.eyJhIjoxfQ.", true},
		{"not.a.jwt", false},
		{"eyJ.a.b", false},
		{"home", false},
		{"", false},
		{"3.86.0", false},
		{"my.file.name.txt", false},
	}
	for _, tc := range tests {
		if got := IsJWT(tc.in); got != tc.want {
			t.Errorf("IsJWT(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestJSONDepthLimit(t *testing.T) {
	deep := strings.Repeat(`{"a":`, maxDepth+5) + `1` + strings.Repeat(`}`, maxDepth+5)
	got := JSON([]byte(deep))
	// Too deep to walk: it degrades to the text path and must not panic.
	if got.Data == nil {
		t.Fatal("Data must never be nil")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestJSONIsIdempotent guards the two-pass path: internal/cli redacts an
// envelope's data before --path is evaluated and internal/output redacts again
// as a backstop, so a second pass over an already-masked body must report no
// change (otherwise every such envelope carries a duplicate warning).
func TestJSONIsIdempotent(t *testing.T) {
	const body = `{"id":1,"apiKey":"paycli-dev-key-deadbeefdeadbeef","sessions":[{"id":"a"}],"title":"ok"}`
	first := JSON([]byte(body))
	if !first.Changed {
		t.Fatalf("first pass did not redact: %s", first.Data)
	}
	second := JSON(first.Data)
	if second.Changed {
		t.Errorf("second pass reported a change: paths=%v\n%s", second.Paths, second.Data)
	}
	if string(second.Data) != string(first.Data) {
		t.Errorf("second pass altered the bytes:\n%s\n%s", first.Data, second.Data)
	}
}

// TestTokenExpIsNotASecret pins §5.3's one internal conflict: the catch-all
// `(?i)token|secret` key matcher would mask token_exp, which the same section
// requires `pay auth status` to print.
func TestTokenExpIsNotASecret(t *testing.T) {
	for _, key := range []string{"token_exp", "tokenExp", "TOKEN_EXP"} {
		if Key(key) {
			t.Errorf("Key(%q) = true; an expiry timestamp is not a credential", key)
		}
	}
	for _, key := range []string{"token", "refreshToken", "token_value", "client_secret", "apiKey"} {
		if !Key(key) {
			t.Errorf("Key(%q) = false; it must still be masked", key)
		}
	}
	res := JSON([]byte(`{"token":"abc","token_exp":"2026-09-17T17:00:00Z"}`))
	if !strings.Contains(string(res.Data), "2026-09-17T17:00:00Z") {
		t.Errorf("token_exp was masked: %s", res.Data)
	}
	if strings.Contains(string(res.Data), "abc") {
		t.Errorf("token survived: %s", res.Data)
	}
}
