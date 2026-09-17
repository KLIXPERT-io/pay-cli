package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/KLIXPERT-io/pay-cli/internal/apierr"
)

const fixtureKey = "paycli-dev-key-deadbeefdeadbeef"

const fixtureJWT = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJpZCI6MX0.ZmFrZXNpZ25hdHVyZQ"

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"", slog.LevelInfo, false},
		{"info", slog.LevelInfo, false},
		{"debug", slog.LevelDebug, false},
		{"DEBUG", slog.LevelDebug, false},
		{" warn ", slog.LevelWarn, false},
		{"warning", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"trace", 0, true},
	}
	for _, tc := range tests {
		got, err := ParseLevel(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("ParseLevel(%q) err = %v", tc.in, err)
		}
		if err != nil {
			e, _ := apierr.As(err)
			if e.Code != apierr.CodeInvalidOption || e.Exit != 5 {
				t.Errorf("got %s/%d, want invalid_option/5", e.Code, e.Exit)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseFormat(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", FormatText, false},
		{"text", FormatText, false},
		{"json", FormatJSON, false},
		{"JSON", FormatJSON, false},
		{"yaml", "", true},
	}
	for _, tc := range tests {
		got, err := ParseFormat(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("ParseFormat(%q) err = %v", tc.in, err)
		}
		if err == nil && got != tc.want {
			t.Errorf("ParseFormat(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLevelFiltering(t *testing.T) {
	tests := []struct {
		name      string
		opts      Options
		wantDebug bool
		wantInfo  bool
		wantError bool
	}{
		{name: "default is info", opts: Options{}, wantInfo: true, wantError: true},
		{name: "debug", opts: Options{Level: LevelDebug}, wantDebug: true, wantInfo: true, wantError: true},
		{name: "verbose forces debug", opts: Options{Verbose: true}, wantDebug: true, wantInfo: true, wantError: true},
		{name: "quiet forces error", opts: Options{Quiet: true}, wantError: true},
		{name: "quiet beats verbose", opts: Options{Quiet: true, Verbose: true}, wantError: true},
		{name: "error level", opts: Options{Level: LevelError}, wantError: true},
		{name: "an invalid level degrades to info", opts: Options{Level: "nope"}, wantInfo: true, wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			opts := tc.opts
			opts.Output = &buf
			l := New(opts)
			l.Debug("dmsg")
			l.Info("imsg")
			l.Error("emsg")
			s := buf.String()
			if strings.Contains(s, "dmsg") != tc.wantDebug {
				t.Errorf("debug present = %v, want %v", strings.Contains(s, "dmsg"), tc.wantDebug)
			}
			if strings.Contains(s, "imsg") != tc.wantInfo {
				t.Errorf("info present = %v, want %v", strings.Contains(s, "imsg"), tc.wantInfo)
			}
			if strings.Contains(s, "emsg") != tc.wantError {
				t.Errorf("error present = %v, want %v", strings.Contains(s, "emsg"), tc.wantError)
			}
		})
	}
}

func TestJSONFormat(t *testing.T) {
	var buf bytes.Buffer
	New(Options{Output: &buf, Format: FormatJSON}).Info("hello", "collection", "pages")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}
	if rec["msg"] != "hello" || rec["collection"] != "pages" {
		t.Errorf("record = %v", rec)
	}
}

func TestNilOutputDiscards(t *testing.T) {
	// Must not panic and must not write anywhere.
	New(Options{}).Info("x")
	Discard().Error("y")
}

// TestSecretsNeverReachTheLog is the §5.3 / §3.1 assertion for this package:
// debug logging is the documented way to see what PayCLI sent, and it must
// stay safe to paste into an issue.
func TestSecretsNeverReachTheLog(t *testing.T) {
	tests := []struct {
		name string
		log  func(*slog.Logger)
	}{
		{
			name: "an Authorization header attribute",
			log: func(l *slog.Logger) {
				l.Debug("request", "Authorization", "users API-Key "+fixtureKey)
			},
		},
		{
			name: "a whole http.Header map",
			log: func(l *slog.Logger) {
				l.Debug("request", "headers", http.Header{
					"Authorization": {"JWT " + fixtureJWT},
					"Cookie":        {"payload-token=" + fixtureJWT},
					"Content-Type":  {"application/json"},
				})
			},
		},
		{
			name: "an apiKey attribute",
			log:  func(l *slog.Logger) { l.Debug("doc", "apiKey", fixtureKey) },
		},
		{
			name: "a token attribute",
			log:  func(l *slog.Logger) { l.Debug("login", "token", fixtureJWT) },
		},
		{
			name: "a url with userinfo",
			log:  func(l *slog.Logger) { l.Debug("get", URL("url", "http://admin:hunter2@h/api?api-key="+fixtureKey)) },
		},
		{
			name: "a JWT inside a message",
			log:  func(l *slog.Logger) { l.Debug("token is " + fixtureJWT) },
		},
		{
			name: "a decoded body map",
			log: func(l *slog.Logger) {
				l.Debug("body", "doc", map[string]any{"id": 1, "apiKey": fixtureKey, "hash": "h"})
			},
		},
		{
			name: "an error carrying a URL",
			log: func(l *slog.Logger) {
				l.Error("failed", "err", errors.New(`Get "http://admin:hunter2@h/api": refused`))
			},
		},
		{
			name: "a group",
			log: func(l *slog.Logger) {
				l.Debug("req", slog.Group("http", "method", "GET", "apiKey", fixtureKey))
			},
		},
		{
			name: "a literal secret under an innocent key",
			log:  func(l *slog.Logger) { l.Debug("resolved", "note", "key is "+fixtureKey) },
		},
	}
	for _, format := range []string{FormatText, FormatJSON} {
		for _, tc := range tests {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				var buf bytes.Buffer
				tc.log(New(Options{Output: &buf, Level: LevelDebug, Format: format, Secrets: []string{fixtureKey}}))
				s := buf.String()
				if s == "" {
					t.Fatal("nothing was logged")
				}
				for _, secret := range []string{fixtureKey, fixtureJWT, "hunter2"} {
					if strings.Contains(s, secret) {
						t.Errorf("log leaked %q:\n%s", secret, s)
					}
				}
			})
		}
	}
}

// TestAuthorizationUsesTheFingerprintForm pins §5.3's hard transport rule: the
// log line proves WHICH credential was used without revealing it.
func TestAuthorizationUsesTheFingerprintForm(t *testing.T) {
	var buf bytes.Buffer
	New(Options{Output: &buf, Level: LevelDebug}).
		Debug("request", "Authorization", "users API-Key "+fixtureKey)
	if !strings.Contains(buf.String(), "<redacted:fp=") {
		t.Errorf("want the fingerprint form, got:\n%s", buf.String())
	}
}

func TestNonSecretAttributesSurvive(t *testing.T) {
	var buf bytes.Buffer
	New(Options{Output: &buf, Level: LevelDebug, Format: FormatJSON}).
		Debug("request", "method", "GET", "collection", "pages", "status", 200, "duration_ms", 41)
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["method"] != "GET" || rec["collection"] != "pages" {
		t.Errorf("useful attributes were mangled: %v", rec)
	}
	if rec["status"] != float64(200) || rec["duration_ms"] != float64(41) {
		t.Errorf("numeric attributes were mangled: %v", rec)
	}
}

func TestURLHelper(t *testing.T) {
	a := URL("url", "http://user:pass@h/api?token=abc")
	if strings.Contains(a.Value.String(), "pass") || strings.Contains(a.Value.String(), "abc") {
		t.Errorf("URL() leaked: %q", a.Value.String())
	}
	if a.Key != "url" {
		t.Errorf("key = %q", a.Key)
	}
}
