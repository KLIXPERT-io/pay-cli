package apierr

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestCheckID is §11.3's first pre-empted 500 plus §9.3's tri-state rule: a
// client-side rejection built on a fact PayCLI never learned is a fabricated
// error, so id_type "unknown" must always send the request.
func TestCheckID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		idType  string
		wantErr bool
	}{
		{"numeric id on a number collection", "66", IDTypeNumber, false},
		{"negative numeric id", "-1", IDTypeNumber, false},
		{"objectid on a number collection", "66f1a2b3c4d5e6f708192a3b", IDTypeNumber, true},
		{"empty id on a number collection", "", IDTypeNumber, true},
		{"float on a number collection", "1.5", IDTypeNumber, true},
		{"objectid on a string collection", "66f1a2b3c4d5e6f708192a3b", IDTypeString, false},
		{"uuid on a string collection", "b3f1a2b3-c4d5-e6f7-0819-2a3b4c5d6e7f", IDTypeString, false},
		{"numeric id on a string collection is fine", "66", IDTypeString, false},
		{"empty id on a string collection", "  ", IDTypeString, true},
		{"unknown id type never rejects an objectid", "66f1a2b3c4d5e6f708192a3b", IDTypeUnknown, false},
		{"unknown id type never rejects anything", "anything at all", IDTypeUnknown, false},
		{"undiscovered id type never rejects", "anything", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckID("pages", tc.id, tc.idType)
			if (got != nil) != tc.wantErr {
				t.Fatalf("CheckID() = %v, wantErr %v", got, tc.wantErr)
			}
			if got != nil {
				if got.Code != CodeInvalidID || got.Exit != 5 {
					t.Errorf("got %s/%d, want invalid_id/5", got.Code, got.Exit)
				}
				if got.Hint == "" {
					t.Error("invalid_id must name the remedy")
				}
			}
		})
	}
}

func TestCheckVersions(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name    string
		has     *bool
		wantErr bool
	}{
		{"versions enabled", &yes, false},
		{"versions disabled", &no, true},
		{"not discovered: send the request", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckVersions("pages", tc.has)
			if (got != nil) != tc.wantErr {
				t.Fatalf("CheckVersions() = %v, wantErr %v", got, tc.wantErr)
			}
			if got != nil && (got.Code != CodeFeatureUnavailable || got.Exit != 10) {
				t.Errorf("got %s/%d, want feature_unavailable/10", got.Code, got.Exit)
			}
		})
	}
}

func TestCauses500(t *testing.T) {
	t.Run("relationship targets rank first", func(t *testing.T) {
		got := Causes500("POST", "/api/posts", []Relationship{{Path: "heroImage", Collection: "media", ID: 4}})
		if len(got) == 0 {
			t.Fatal("no causes")
		}
		if got[0].Cause != "relationship_target_missing" {
			t.Errorf("causes[0] = %q", got[0].Cause)
		}
		if got[0].Confidence != CauseMedium {
			t.Errorf("confidence = %q, want medium", got[0].Confidence)
		}
		if got[0].Fix != "pay get media 4" {
			t.Errorf("fix = %q, want a runnable command", got[0].Fix)
		}
	})
	t.Run("a read still explains the mask", func(t *testing.T) {
		got := Causes500("GET", "/api/pages", nil)
		last := got[len(got)-1]
		if last.Cause != "message_masked_by_config" {
			t.Errorf("last cause = %q", last.Cause)
		}
		for _, c := range got {
			if c.Cause == "hook_threw" {
				t.Error("a GET cannot have run a beforeChange hook")
			}
		}
	})
	t.Run("a write mentions hooks", func(t *testing.T) {
		got := Causes500("PATCH", "/api/pages/1", nil)
		var found bool
		for _, c := range got {
			if c.Cause == "hook_threw" {
				found = true
			}
		}
		if !found {
			t.Error("a write should list hook_threw")
		}
	})
	t.Run("every cause has a confidence and a fix", func(t *testing.T) {
		for _, c := range Causes500("POST", "/api/x", []Relationship{{Path: "a", ID: 1}}) {
			if c.Confidence == "" || c.Fix == "" {
				t.Errorf("incomplete cause %+v", c)
			}
		}
	})
}

func TestDidYouMean(t *testing.T) {
	collections := []string{"pages", "posts", "media", "users", "categories", "form-submissions"}
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"singular slug", "page", []string{"pages"}},
		{"typo", "pagez", []string{"pages"}},
		{"transposition", "psots", []string{"posts"}},
		{"prefix", "categor", []string{"categories"}},
		{"exact", "media", []string{"media"}},
		{"nothing close", "zzzzzzzzzzzz", []string{}},
		{"empty input", "", []string{}},
		{"case insensitive", "PAGES", []string{"pages"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DidYouMean(tc.input, collections)
			if len(tc.want) == 0 {
				if len(got) != 0 {
					t.Errorf("DidYouMean(%q) = %v, want none", tc.input, got)
				}
				return
			}
			if len(got) == 0 || got[0] != tc.want[0] {
				t.Errorf("DidYouMean(%q) = %v, want %v first", tc.input, got, tc.want[0])
			}
		})
	}
	t.Run("never nil", func(t *testing.T) {
		if got := DidYouMean("x", nil); got == nil {
			t.Error("DidYouMean must return [] rather than nil")
		}
	})
	t.Run("capped", func(t *testing.T) {
		many := []string{"page", "pagea", "pageb", "pagec", "paged"}
		if got := DidYouMean("page", many); len(got) > maxSuggestions {
			t.Errorf("returned %d suggestions, want <= %d", len(got), maxSuggestions)
		}
	})
	t.Run("deterministic", func(t *testing.T) {
		a := DidYouMean("pagez", collections)
		b := DidYouMean("pagez", collections)
		if diff := cmp.Diff(a, b); diff != "" {
			t.Errorf("not deterministic:\n%s", diff)
		}
	})
}

func TestLevenshtein(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"a", "", 1},
		{"", "abc", 3},
		{"pages", "pages", 0},
		{"page", "pages", 1},
		{"psots", "posts", 2},
		{"kitten", "sitting", 3},
		{"héllo", "hello", 1},
	}
	for _, tc := range tests {
		if got := levenshtein(tc.a, tc.b); got != tc.want {
			t.Errorf("levenshtein(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestNetwork(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode Code
	}{
		{"nil", nil, ""},
		{"dns", &net.DNSError{Err: "no such host", Name: "nope.invalid", IsNotFound: true}, CodeDNSFailure},
		{"timeout net.Error", timeoutErr{}, CodeTimeout},
		{"context deadline", errors.New("Get \"http://h\": context deadline exceeded"), CodeTimeout},
		{"tls", fmt.Errorf("tls: failed handshake: %w", x509.UnknownAuthorityError{}), CodeTLSError},
		{"certificate", errors.New("x509: certificate signed by unknown authority"), CodeTLSError},
		{"refused", errors.New("dial tcp 127.0.0.1:3900: connect: connection refused"), CodeNetworkUnreachable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Network(tc.err)
			if tc.wantCode == "" {
				if got != nil {
					t.Fatalf("Network(nil) = %v", got)
				}
				return
			}
			if got.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q (message %q)", got.Code, tc.wantCode, got.Message)
			}
			if got.Exit != ExitNetwork {
				t.Errorf("Exit = %d, want %d", got.Exit, ExitNetwork)
			}
			if !errors.Is(got, tc.err) {
				t.Error("the original error must stay in the chain")
			}
			if got.Hint == "" {
				t.Error("a network failure must name its remedy")
			}
		})
	}
}

// TestNetworkRedactsTheURL: a transport error string embeds the request URL,
// which may carry userinfo.
func TestNetworkRedactsTheURL(t *testing.T) {
	err := errors.New(`Get "http://admin:hunter2@localhost:3900/api/pages": connection refused`)
	got := Network(err)
	if strings.Contains(got.Message, "hunter2") {
		t.Errorf("message leaked a credential: %q", got.Message)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o deadline reached" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
